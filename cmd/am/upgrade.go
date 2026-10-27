package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// cmdUpgrade moves the app to another framework version (§19): go get, regenerate, and
// plan a migration for whatever the new version generates differently (policies,
// indexes), which am migrate plan detects by the schema hash it records. Code changes a
// version needs are made by the regeneration; hand-written code is left for am check to
// point at.
func cmdUpgrade(args []string) error {
	fs, _, err := flags("upgrade", args, nil)
	if err != nil {
		return err
	}
	version := fs.Arg(0)
	if version == "" {
		version = "latest"
	}
	steps := [][]string{
		{"go", "get", "github.com/abhilashiig/ambit@" + version},
		{"go", "mod", "tidy"},
	}
	// An app built against a local checkout (a replace in go.mod) already has the new
	// source; there is no version to fetch.
	if out, err := exec.Command("go", "list", "-m", "-f", "{{if .Replace}}{{.Replace.Path}}{{end}}", "github.com/abhilashiig/ambit").Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		fmt.Println("github.com/abhilashiig/ambit is replaced by", strings.TrimSpace(string(out)), "- regenerating against it")
		steps = nil
	}
	for _, s := range steps {
		cmd := exec.Command(s[0], s[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%v: %w", s, err)
		}
	}
	// Regenerate with the new framework: run the new am, not this one.
	gen := exec.Command("go", "run", "github.com/abhilashiig/ambit/cmd/am", "gen")
	gen.Stdout, gen.Stderr = os.Stdout, os.Stderr
	if err := gen.Run(); err != nil {
		return fmt.Errorf("am gen with the new version: %w", err)
	}
	if all, _ := migrations(); len(all) > 0 {
		plan := exec.Command("go", "run", "github.com/abhilashiig/ambit/cmd/am", "migrate", "plan", "--name", "framework_upgrade")
		plan.Stdout, plan.Stderr = os.Stdout, os.Stderr
		if err := plan.Run(); err != nil {
			return err
		}
	}
	fmt.Println("upgraded; now: am check --strict, am test, am diff --base main")
	return nil
}
