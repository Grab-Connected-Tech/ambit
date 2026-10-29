package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiffRefusesAStaleModel is finding one of the review: a spec edited without
// am gen must not read as "no behavior change". It runs on a throwaway copy of the
// repository so the example app other packages build is never touched.
func TestDiffRefusesAStaleModel(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the example app's spec")
	}
	repo, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	archive := exec.Command("sh", "-c", "git archive HEAD | tar -x -C "+dir)
	archive.Dir = strings.TrimSpace(string(repo))
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("archive: %v\n%s", err, out)
	}
	git("init", "-q")
	git("add", "-A")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "base")
	t.Chdir(filepath.Join(dir, "example", "myapp"))

	if err := cmdDiff([]string{"--base", "HEAD"}); err != nil {
		t.Fatalf("an unchanged app must diff clean: %v", err)
	}
	spec := filepath.Join("app", "billing", "spec.go")
	src, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(src), `s.FieldRead("tax_id", app.Admin)`, `s.FieldRead("tax_id", app.Admin.Or(app.Accountant))`, 1)
	if widened == string(src) {
		t.Fatal("the example spec no longer has the line this test widens")
	}
	if err := os.WriteFile(spec, []byte(widened), 0o644); err != nil {
		t.Fatal(err)
	}
	var exit exitError
	if err := cmdDiff([]string{"--base", "HEAD"}); !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("a spec changed without am gen must fail the diff, got %v", err)
	}
}
