// Package run applies planned migrations to a database (§13.1). It reads the plan files
// from any fs.FS, so am migrate reads migrations/ on disk and an app binary reads the
// copy embedded in it.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Grab-Connected-Tech/ambit/am"
	"github.com/Grab-Connected-Tech/ambit/migrate"
)

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.plan\.json$`)

// Migration is one planned migration: its plan file and that file's checksum.
type Migration struct {
	Version, Name, Path string
	Plan                migrate.Plan
	Checksum            string
}

// List reads the plan files in version order.
func List(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		mg := Migration{Version: m[1], Name: m[2], Path: e.Name(), Checksum: checksum(raw)}
		if err := json.Unmarshal(raw, &mg.Plan); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, mg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func checksum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type appliedRow struct{ Checksum, ModelHash string }

func applied(ctx context.Context, conn *pgx.Conn) (map[string]appliedRow, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum, model_hash FROM am_migrations`)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "42P01" {
			return map[string]appliedRow{}, nil // nothing applied yet
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]appliedRow{}
	for rows.Next() {
		var v string
		var r appliedRow
		if err := rows.Scan(&v, &r.Checksum, &r.ModelHash); err != nil {
			return nil, err
		}
		out[v] = r
	}
	return out, rows.Err()
}

// StatusRow is one migration's state in a database.
type StatusRow struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	State   string `json:"state"` // applied|pending|modified
}

// Status compares the plan files with what the database has applied.
func Status(ctx context.Context, conn *pgx.Conn, fsys fs.FS) ([]StatusRow, []Migration, error) {
	all, err := List(fsys)
	if err != nil {
		return nil, nil, err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	var out []StatusRow
	for _, m := range all {
		r := StatusRow{Version: m.Version, Name: m.Name, State: "pending"}
		if a, ok := done[m.Version]; ok {
			r.State = "applied"
			if a.Checksum != m.Checksum {
				r.State = "modified"
			}
		}
		out = append(out, r)
	}
	return out, all, nil
}

// Modified is the AM-MIG-002 refusal for an applied plan that was edited.
func Modified(r StatusRow) error {
	return fmt.Errorf("AM-MIG-002: applied migration %s_%s was edited; applied migrations are immutable, plan a new one instead", r.Version, r.Name)
}

// Apply runs every pending migration in order and returns the ones it ran. allowed holds
// the flags a plan's steps may require (--allow-destructive, --plan); each must be given
// before anything runs (AM-MIG-001).
func Apply(ctx context.Context, conn *pgx.Conn, fsys fs.FS, allowed map[string]bool) ([]string, error) {
	rows, all, err := Status(ctx, conn, fsys)
	if err != nil {
		return nil, err
	}
	var ran []string
	for i, r := range rows {
		switch r.State {
		case "modified":
			return ran, Modified(r)
		case "applied":
			continue
		}
		m := all[i]
		for _, s := range m.Plan.Steps {
			if s.NeedsFlag != "" && !allowed[s.NeedsFlag] {
				return ran, fmt.Errorf("AM-MIG-001: %s_%s step %q (%s) needs %s", m.Version, m.Name, s.Detail, s.Class, s.NeedsFlag)
			}
			if s.Backfill != nil && strings.TrimSpace(s.Backfill.Set) == "" {
				return ran, fmt.Errorf("%s_%s step %q backfills %s.%s: write the value each existing row gets into the plan's backfill.set",
					m.Version, m.Name, s.Detail, s.Backfill.Table, s.Backfill.Column)
			}
		}
		if err := ApplyOne(ctx, conn, m, allowed["--allow-destructive"]); err != nil {
			return ran, fmt.Errorf("%s_%s: %w", m.Version, m.Name, err)
		}
		ran = append(ran, m.Version+"_"+m.Name)
	}
	return ran, nil
}

// ApplyOne runs a plan's transactional statements under lock and statement timeouts,
// retrying lock timeouts with backoff, then the statements that cannot run in a
// transaction (§13.1 step 4).
func ApplyOne(ctx context.Context, conn *pgx.Conn, m Migration, destructive bool) error {
	var inTx, outside []string
	for _, s := range m.Plan.Steps {
		inTx = append(inTx, s.InTx...)
		outside = append(outside, s.OutsideTx...)
	}
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			for _, stmt := range append([]string{
				"SET LOCAL lock_timeout = '2s'", "SET LOCAL statement_timeout = '15min'"}, inTx...) {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("%w\n  in: %s", err, firstLine(stmt))
				}
			}
			return nil
		})
		var pg *pgconn.PgError
		if err == nil {
			break
		}
		if !errors.As(err, &pg) || pg.Code != "55P03" || attempt == 5 {
			return err
		}
		time.Sleep(time.Duration(attempt*attempt) * 500 * time.Millisecond)
	}
	// Backfills run between the expand statements and the constraints that need them.
	for _, s := range m.Plan.Steps {
		if s.Backfill != nil {
			if err := backfill(ctx, conn, m.Version, *s.Backfill); err != nil {
				return fmt.Errorf("backfill %s.%s: %w", s.Backfill.Table, s.Backfill.Column, err)
			}
		}
	}
	for _, stmt := range outside {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w\n  in: %s", err, firstLine(stmt))
		}
	}
	_, err := conn.Exec(ctx,
		`INSERT INTO am_migrations (version, name, checksum, model_hash, destructive_allowed) VALUES ($1, $2, $3, $4, $5)`,
		m.Version, m.Name, m.Checksum, m.Plan.ModelHash, destructive)
	return err
}

// backfill fills a column's nulls in batches keyed by primary key, one transaction per
// batch, recording the cursor in am_backfills so an interrupted run resumes where it
// stopped rather than starting over (§13.3).
func backfill(ctx context.Context, conn *pgx.Conn, version string, b migrate.Backfill) error {
	if b.Batch <= 0 {
		b.Batch = 1000
	}
	// The migration role writes every tenant's rows.
	if _, err := conn.Exec(ctx, "SELECT set_config('am.cross_tenant', 'on', false)"); err != nil {
		return err
	}
	var cursor string
	err := conn.QueryRow(ctx, `INSERT INTO am_backfills (id, version, column_name, cursor) VALUES ($1, $2, $3, '""')
		ON CONFLICT (version, column_name) DO UPDATE SET updated_at = now()
		RETURNING coalesce(cursor #>> '{}', '')`, am.NewID(), version, b.Table+"."+b.Column).Scan(&cursor)
	if err != nil {
		return err
	}
	table, col := quoteIdent(b.Table), quoteIdent(b.Column)
	for {
		var last string
		var n int64
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, fmt.Sprintf(`WITH batch AS (
				SELECT id FROM %[1]s WHERE %[2]s IS NULL AND ($1 = '' OR id > $1::uuid) ORDER BY id LIMIT %[3]d FOR UPDATE)
				, done AS (UPDATE %[1]s t SET %[2]s = %[4]s FROM batch WHERE t.id = batch.id RETURNING t.id)
				SELECT coalesce(max(id::text), ''), count(*) FROM done`, table, col, b.Batch, b.Set), cursor).Scan(&last, &n)
			if err != nil || n == 0 {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE am_backfills SET cursor = to_jsonb($3::text), rows_done = rows_done + $4, state = 'running', updated_at = now()
				WHERE version = $1 AND column_name = $2`, version, b.Table+"."+b.Column, last, n)
			return err
		})
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		cursor = last
	}
	_, err = conn.Exec(ctx, `UPDATE am_backfills SET state = 'done', updated_at = now() WHERE version = $1 AND column_name = $2`,
		version, b.Table+"."+b.Column)
	return err
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// Main is what an app binary runs for "myapp migrate apply|status" at release time
// (§18.3): the migrations it applies are the ones embedded in it, so the schema and the
// code that expects it ship together. It returns the process exit code.
func Main(ctx context.Context, fsys fs.FS, databaseURL string, args []string) int {
	allowed := map[string]bool{}
	cmd := ""
	for _, a := range args {
		switch a {
		case "--allow-destructive", "--plan":
			allowed[a] = true
		default:
			cmd = a
		}
	}
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "migrate: set AM_DATABASE_URL")
		return 2
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	defer conn.Close(ctx)
	switch cmd {
	case "apply":
		ran, err := Apply(ctx, conn, fsys, allowed)
		for _, r := range ran {
			fmt.Println("applied", r)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			return 1
		}
		if len(ran) == 0 {
			fmt.Println("up to date")
		}
	case "status":
		rows, _, err := Status(ctx, conn, fsys)
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			return 1
		}
		code := 0
		for _, r := range rows {
			fmt.Printf("%s_%s  %s\n", r.Version, r.Name, r.State)
			if r.State != "applied" {
				code = 3 // pending or modified: not safe to start new instances yet
			}
		}
		return code
	default:
		fmt.Fprintln(os.Stderr, "usage: migrate apply [--allow-destructive] [--plan] | status")
		return 2
	}
	return 0
}
