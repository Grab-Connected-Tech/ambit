package codegen

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/abhilashiig/ambit/am"
)

// The committed example app is the fixture: generating it again must leave every file
// byte-identical, the result must build, and its schema must apply to Postgres.
func TestGenerateExampleIsStableBuildsAndApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go tool")
	}
	dir := filepath.Join("..", "example", "myapp")
	opts := Options{Dir: dir, AppPackage: "github.com/abhilashiig/ambit/example/myapp/app"}
	res, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, f := range res.Files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		before[f] = raw
	}
	if _, err := Generate(opts); err != nil {
		t.Fatal(err)
	}
	for f, raw := range before {
		now, _ := os.ReadFile(filepath.Join(dir, f))
		if !bytes.Equal(raw, now) {
			t.Errorf("%s changed on a second generation", f)
		}
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("generated app does not build: %v\n%s", err, out)
	}

	url := os.Getenv("AM_TEST_DB_URL")
	if url == "" {
		t.Log("AM_TEST_DB_URL unset; schema not applied")
		return
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "am_codegen_" + strings.ReplaceAll(string(am.NewID()), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	if _, err := db.Exec(ctx, string(before["migrations/schema.gen.sql"])); err != nil {
		t.Fatalf("schema.gen.sql does not apply: %v", err)
	}
}
