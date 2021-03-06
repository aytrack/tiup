package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/c4pt0r/tiup/components/playground/instance"
	"github.com/c4pt0r/tiup/pkg/localdata"
	"github.com/c4pt0r/tiup/pkg/meta"
	_ "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
)

func installIfMissing(profile *localdata.Profile, component, version string) error {
	versions, err := profile.InstalledVersions(component)
	if err != nil {
		return err
	}
	if len(versions) > 0 {
		if meta.Version(version).IsEmpty() {
			return nil
		}
		found := false
		for _, v := range versions {
			if v == version {
				found = true
				break
			}
		}
		if found {
			return nil
		}
	}
	spec := component
	if !meta.Version(version).IsEmpty() {
		spec = fmt.Sprintf("%s:%s", component, version)
	}
	c := exec.Command("tiup", "install", spec)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func execute() error {
	tidbNum := 1
	tikvNum := 1
	pdNum := 1
	host := "127.0.0.1"

	rootCmd := &cobra.Command{
		Use:          "playground",
		Short:        "Bootstrap a TiDB cluster in your local host",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			version := ""
			if len(args) > 0 {
				version = args[0]
			}
			return bootCluster(version, pdNum, tidbNum, tikvNum, host)
		},
	}

	rootCmd.Flags().IntVarP(&tidbNum, "db", "", 1, "TiDB instance number")
	rootCmd.Flags().IntVarP(&tikvNum, "kv", "", 1, "TiKV instance number")
	rootCmd.Flags().IntVarP(&pdNum, "pd", "", 1, "PD instance number")
	rootCmd.Flags().StringVarP(&host, "host", "", host, "Playground cluster host")

	return rootCmd.Execute()
}

func tryConnect(dsn string) error {
	cli, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer cli.Close()

	conn, err := cli.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()

	return nil
}

func checkDB(dbAddr string) {
	dsn := fmt.Sprintf("root:@tcp(%s)/", dbAddr)
	for i := 0; i < 60; i++ {
		if err := tryConnect(dsn); err != nil {
			time.Sleep(time.Second)
		} else {
			ss := strings.Split(dbAddr, ":")
			fmt.Printf("To connect TiDB: mysql --host %s --port %s -u root\n", ss[0], ss[1])
			break
		}
	}
}

func hasDashboard(pdAddr string) bool {
	resp, err := http.Get(fmt.Sprintf("http://%s/dashboard", pdAddr))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return true
	}
	return false
}

func bootCluster(version string, pdNum, tidbNum, tikvNum int, host string) error {
	if pdNum < 1 || tidbNum < 1 || tikvNum < 1 {
		return fmt.Errorf("all components count must be great than 0 (tidb=%v, tikv=%v, pd=%v)",
			tidbNum, tikvNum, pdNum)
	}

	// Initialize the profile
	profileRoot := os.Getenv(localdata.EnvNameHome)
	if profileRoot == "" {
		return fmt.Errorf("cannot read environment variable %s", localdata.EnvNameHome)
	}
	profile := localdata.NewProfile(profileRoot)
	for _, comp := range []string{"pd", "tikv", "tidb"} {
		if err := installIfMissing(profile, comp, version); err != nil {
			return err
		}
	}

	dataDir := os.Getenv(localdata.EnvNameInstanceDataDir)
	if dataDir == "" {
		return fmt.Errorf("cannot read environment variable %s", localdata.EnvNameInstanceDataDir)
	}

	all := make([]instance.Instance, 0, pdNum+tikvNum+tidbNum)
	pds := make([]*instance.PDInstance, 0, pdNum)
	kvs := make([]*instance.TiKVInstance, 0, tikvNum)
	dbs := make([]*instance.TiDBInstance, 0, tidbNum)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < pdNum; i++ {
		dir := filepath.Join(dataDir, fmt.Sprintf("pd-%d", i))
		inst := instance.NewPDInstance(dir, host, i)
		pds = append(pds, inst)
		all = append(all, inst)
	}
	for _, pd := range pds {
		pd.Join(pds)
	}

	for i := 0; i < tikvNum; i++ {
		dir := filepath.Join(dataDir, fmt.Sprintf("tikv-%d", i))
		inst := instance.NewTiKVInstance(dir, host, i, pds)
		kvs = append(kvs, inst)
		all = append(all, inst)
	}

	for i := 0; i < tidbNum; i++ {
		dir := filepath.Join(dataDir, fmt.Sprintf("tidb-%d", i))
		inst := instance.NewTiDBInstance(dir, host, i, pds)
		dbs = append(dbs, inst)
		all = append(all, inst)
	}

	fmt.Println("Playground Bootstrapping...")

	for _, inst := range all {
		if err := inst.Start(ctx, meta.Version(version)); err != nil {
			return err
		}
	}

	for _, db := range dbs {
		checkDB(db.Addr())
	}

	if pdAddr := pds[0].Addr(); hasDashboard(pdAddr) {
		fmt.Printf("To view the dashboard: http://%s/dashboard\n", pdAddr)
	}

	dumpDSN(dbs)
	setupSignalHandler(func(bool) {
		cleanDSN()
	})

	for _, inst := range all {
		if err := inst.Wait(); err != nil {
			return err
		}
	}
	return nil
}

func dumpDSN(dbs []*instance.TiDBInstance) {
	dsn := []string{}
	for _, db := range dbs {
		dsn = append(dsn, fmt.Sprintf("mysql://root@%s", db.Addr()))
	}
	ioutil.WriteFile("dsn", []byte(strings.Join(dsn, "\n")), 0644)
}

func cleanDSN() {
	os.Remove("dsn")
}

// SetupSignalHandler setup signal handler for TiDB Server
func setupSignalHandler(shutdownFunc func(bool)) {
	//todo deal with dump goroutine stack on windows
	closeSignalChan := make(chan os.Signal, 1)
	signal.Notify(closeSignalChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	go func() {
		sig := <-closeSignalChan
		shutdownFunc(sig == syscall.SIGQUIT)
	}()
}

func main() {
	if err := execute(); err != nil {
		fmt.Println("Playground bootstrapping failed:", err)
		os.Exit(1)
	}
}
