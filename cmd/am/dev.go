package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/abhilashiig/ambit/runtime"
)

// cmdDev runs the app with AM_DEV=1 (which turns on /_am introspection), reseeds it, and
// regenerates, rebuilds and restarts whenever a source file changes (§17.4).
func cmdDev(args []string) error {
	var addr *string
	var mcp *bool
	_, _, err := flags("dev", args, func(fs *flag.FlagSet) {
		addr = fs.String("addr", ":8080", "listen address")
		mcp = fs.Bool("mcp", false, "serve the introspection API as MCP tools on stdio")
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *mcp {
		// stdout carries the protocol; everything the loop prints goes to stderr.
		proto := os.Stdout
		os.Stdout = os.Stderr
		base := "http://localhost" + (*addr)[strings.LastIndex(*addr, ":"):]
		go func() {
			_ = mcpServe(os.Stdin, proto, base)
			stop() // the client hung up
		}()
	}
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("am-dev-%d", os.Getpid()))
	defer os.Remove(bin)
	for {
		stamp := sourceStamp()
		proc := devStart(ctx, bin, *addr)
		// Wait for a change, an exit or Ctrl-C.
		exited := make(chan struct{})
		if proc != nil {
			go func() { _ = proc.Wait(); close(exited) }()
		}
		tick := time.NewTicker(500 * time.Millisecond)
	wait:
		for {
			select {
			case <-ctx.Done():
				tick.Stop()
				devStop(proc, exited)
				return nil
			case <-exited:
				exited = nil // keep watching: a fix to the source restarts it
			case <-tick.C:
				if sourceStamp() != stamp {
					break wait
				}
			}
		}
		tick.Stop()
		fmt.Fprintln(os.Stderr, "am dev: change detected, restarting")
		devStop(proc, exited)
	}
}

// devStart regenerates, migrates, builds and starts the server; it returns nil when a
// step failed, and the error has been printed.
func devStart(ctx context.Context, bin, addr string) *exec.Cmd {
	if err := cmdGen(nil); err != nil {
		fmt.Fprintln(os.Stderr, "am dev: gen:", err)
		return nil
	}
	if os.Getenv("AM_DATABASE_URL") != "" {
		if all, _ := migrations(); len(all) > 0 {
			if err := migrateApply(ctx, os.Getenv("AM_DATABASE_URL"), map[string]bool{}, false); err != nil {
				fmt.Fprintln(os.Stderr, "am dev: migrate:", err)
				return nil
			}
		} else {
			fmt.Fprintln(os.Stderr, "am dev: no migrations yet; run am migrate plan && am migrate apply")
		}
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/server")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return nil
	}
	proc := exec.Command(bin)
	proc.Env = append(os.Environ(), "AM_DEV=1", "AM_ADDR="+addr)
	if os.Getenv("AM_ENCRYPTION_KEY") == "" {
		key, err := devKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "am dev:", err)
			return nil
		}
		proc.Env = append(proc.Env, "AM_ENCRYPTION_KEY="+key)
	}
	proc.Stdout, proc.Stderr = os.Stdout, os.Stderr
	if err := proc.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "am dev:", err)
		return nil
	}
	base := "http://localhost" + addr[strings.LastIndex(addr, ":"):]
	for i := 0; i < 50; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			r.Body.Close()
			if r, err := http.Post(base+"/_am/reset", "application/json", nil); err == nil {
				r.Body.Close()
				if r.StatusCode != http.StatusOK {
					fmt.Fprintln(os.Stderr, "am dev: seeding failed; see the server log")
				}
			}
			fmt.Fprintf(os.Stderr, "am dev: serving on %s (introspection at %s/_am/actions)\n", addr, base)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return proc
}

func devStop(proc *exec.Cmd, exited chan struct{}) {
	if proc == nil || proc.Process == nil || exited == nil {
		return
	}
	_ = proc.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = proc.Process.Kill()
		<-exited
	}
}

// sourceStamp summarizes every hand-written input: generated files are excluded, or a
// regeneration would restart the server again.
func sourceStamp() string {
	var n int
	var latest time.Time
	_ = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "gen", "client", "migrations", ".git", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql") || strings.HasSuffix(path, ".yaml") {
			if path == filepath.Join("cmd", "server", "main.go") {
				return nil
			}
			n++
			if info, err := d.Info(); err == nil && info.ModTime().After(latest) {
				latest = info.ModTime()
			}
		}
		return nil
	})
	return fmt.Sprintf("%d/%d", n, latest.UnixNano())
}

// devKey is the development encryption key, kept in .am/dev.key so data written in one
// session still opens in the next. It is never used outside am dev.
func devKey() (string, error) {
	path := filepath.Join(".am", "dev.key")
	if raw, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	key := runtime.NewKey()
	if err := amDir(); err != nil {
		return "", err
	}
	return key, os.WriteFile(path, []byte(key+"\n"), 0o600)
}

// amDir makes .am/, the local state am keeps (a dev key, captured queries), and keeps it
// out of version control: both can hold secrets or data.
func amDir() error {
	if err := os.MkdirAll(".am", 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(".am", ".gitignore"), []byte("*\n"), 0o600)
}
