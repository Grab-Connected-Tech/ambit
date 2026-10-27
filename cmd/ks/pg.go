package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// disposablePostgres starts a throwaway Postgres for ks test when no database is
// configured (§16.1): a local server binary in a temporary data directory, or else a
// container. It returns the URL and a function that stops and removes it.
func disposablePostgres() (string, func(), error) {
	if bin := serverBinDir(); bin != "" {
		return localPostgres(bin)
	}
	if _, err := exec.LookPath("docker"); err == nil && exec.Command("docker", "info").Run() == nil {
		return containerPostgres()
	}
	return "", nil, fmt.Errorf("no database: set KS_TEST_DB_URL, or install Postgres server binaries or Docker")
}

// serverBinDir finds a directory holding pg_ctl, initdb and the postgres server itself
// (client-only packages ship pg_ctl without the server).
func serverBinDir() string {
	var dirs []string
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	dirs = append(dirs, "/Applications/Postgres.app/Contents/Versions/latest/bin")
	for _, pattern := range []string{"/usr/lib/postgresql/*/bin", "/opt/homebrew/opt/postgresql@*/bin", "/usr/local/opt/postgresql@*/bin"} {
		m, _ := filepath.Glob(pattern)
		dirs = append(dirs, m...)
	}
	for _, d := range dirs {
		ok := true
		for _, b := range []string{"pg_ctl", "initdb", "postgres"} {
			if _, err := os.Stat(filepath.Join(d, b)); err != nil {
				ok = false
			}
		}
		if ok {
			return d
		}
	}
	return ""
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func localPostgres(bin string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ks-pg-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	data := filepath.Join(dir, "data")
	if out, err := exec.Command(filepath.Join(bin, "initdb"), "-D", data, "-U", "postgres", "-A", "trust", "--no-sync", "-E", "UTF8").CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("initdb: %v\n%s", err, out)
	}
	port, err := freePort()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	// fsync off: the data is thrown away, and tests run several times faster.
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off -c synchronous_commit=off -c full_page_writes=off", port, dir)
	if out, err := exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-o", opts, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("pg_ctl start: %v\n%s", err, out)
	}
	stop := func() {
		_ = exec.Command(filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "stop").Run()
		cleanup()
	}
	return "postgres://postgres@127.0.0.1:" + strconv.Itoa(port) + "/postgres?sslmode=disable", stop, nil
}

func containerPostgres() (string, func(), error) {
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "-e", "POSTGRES_PASSWORD=ks", "-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"postgres:16", "-c", "fsync=off").Output()
	if err != nil {
		return "", nil, fmt.Errorf("docker run postgres: %w", err)
	}
	id := string(out[:12])
	stop := func() { _ = exec.Command("docker", "rm", "-f", id).Run() }
	url := fmt.Sprintf("postgres://postgres:ks@127.0.0.1:%d/postgres?sslmode=disable", port)
	for i := 0; i < 60; i++ {
		if exec.Command("docker", "exec", id, "pg_isready", "-U", "postgres").Run() == nil {
			return url, stop, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	stop()
	return "", nil, fmt.Errorf("the postgres container did not become ready")
}
