package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/migrate"
	"keystone.dev/keystone/model"
)

// A migration is three files under migrations/ sharing a numbered prefix (§13.1):
//
//	0002_add_due_date.plan.json   the classified steps; what apply executes
//	0002_add_due_date.sql         the same statements, for review
//	0002_add_due_date.model.json  the model after this migration; the next plan's base
//
// Applied migrations are immutable: ks_migrations keeps each plan's checksum, and a plan
// file that no longer matches it fails status and apply (KS-MIG-002).
const migrationsDir = "migrations"

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.plan\.json$`)

type migration struct {
	Version, Name, Path string
	Plan                migrate.Plan
	Checksum            string
}

func cmdMigrate(args []string) error {
	var name *string
	var allowDestructive, allowPlan *bool
	fs, asJSON, err := flags("migrate", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "plan: the migration's name")
		allowDestructive = fs.Bool("allow-destructive", false, "apply: allow destructive steps")
		allowPlan = fs.Bool("plan", false, "apply: allow steps that need a reviewed plan")
	})
	if err != nil {
		return err
	}
	allowed := map[string]bool{"--allow-destructive": *allowDestructive, "--plan": *allowPlan}
	switch fs.Arg(0) {
	case "plan":
		return migratePlan(*name, asJSON)
	case "apply":
		url, err := databaseURL("KS_DATABASE_URL")
		if err != nil {
			return err
		}
		return migrateApply(context.Background(), url, allowed, asJSON)
	case "status":
		url, err := databaseURL("KS_DATABASE_URL")
		if err != nil {
			return err
		}
		return migrateStatus(context.Background(), url, asJSON)
	case "verify":
		url, err := databaseURL("KS_TEST_DB_URL", "KS_DATABASE_URL")
		if err != nil {
			return err
		}
		return migrateVerify(context.Background(), url, asJSON)
	}
	return fmt.Errorf("usage: ks migrate plan|verify|apply|status")
}

func databaseURL(vars ...string) (string, error) {
	for _, v := range vars {
		if u := os.Getenv(v); u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("set %s to the database to migrate", vars[0])
}

// migrations lists the planned migrations in order.
func migrations() ([]migration, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		path := filepath.Join(migrationsDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		mg := migration{Version: m[1], Name: m[2], Path: path, Checksum: checksum(raw)}
		if err := json.Unmarshal(raw, &mg.Plan); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
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

func migratePlan(name string, asJSON bool) error {
	head, diags, err := loadModel()
	if err != nil {
		return err
	}
	if hasErrors(diags) {
		return report(diags, asJSON, false)
	}
	all, err := migrations()
	if err != nil {
		return err
	}
	var plan migrate.Plan
	if len(all) == 0 {
		// The first migration is the whole schema.
		plan = migrate.Plan{ModelHash: head.Hash(), Steps: []migrate.Step{{
			Class: migrate.Safe, Subject: head.Name(), Detail: "initial schema", InTx: migrate.DDL(head)}}}
		if name == "" {
			name = "initial"
		}
	} else {
		last := all[len(all)-1]
		raw, err := os.ReadFile(strings.TrimSuffix(last.Path, ".plan.json") + ".model.json")
		if err != nil {
			return fmt.Errorf("the base model of %s is missing: %w", last.Path, err)
		}
		base, err := model.Load(raw)
		if err != nil {
			return err
		}
		plan = migrate.Diff(base, head)
	}
	if plan.Empty() {
		if asJSON {
			printJSON(map[string]any{"planned": false})
		} else {
			fmt.Println("no schema change since the last migration")
		}
		return nil
	}
	if name == "" {
		name = "change"
	}
	name = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(name), "_"), "_")
	plan.Name = name
	prefix := filepath.Join(migrationsDir, fmt.Sprintf("%04d_%s", len(all)+1, name))
	rawPlan, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		prefix + ".plan.json":  append(rawPlan, '\n'),
		prefix + ".sql":        []byte(renderSQL(plan)),
		prefix + ".model.json": head.Bytes(),
	}
	for path, b := range files {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}
	if asJSON {
		printJSON(map[string]any{"planned": true, "plan": prefix + ".plan.json", "maxRisk": plan.MaxRisk(), "steps": plan.Steps})
		return nil
	}
	fmt.Printf("%s.plan.json (max risk %s)\n", prefix, plan.MaxRisk())
	for _, s := range plan.Steps {
		fmt.Printf("  %-16s %s: %s\n", s.Class, s.Subject, s.Detail)
		for _, ph := range s.Phases {
			fmt.Printf("      %s\n", ph)
		}
		if s.NeedsFlag != "" {
			fmt.Printf("      apply needs %s\n", s.NeedsFlag)
		}
	}
	return nil
}

func renderSQL(p migrate.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- %s: generated by ks migrate plan; review the plan file, do not edit\n", p.Name)
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "\n-- [%s] %s: %s\n", s.Class, s.Subject, s.Detail)
		for _, stmt := range s.InTx {
			b.WriteString(strings.TrimSpace(stmt) + "\n")
		}
		if len(s.OutsideTx) > 0 {
			b.WriteString("-- outside the transaction:\n")
			for _, stmt := range s.OutsideTx {
				b.WriteString(strings.TrimSpace(stmt) + "\n")
			}
		}
	}
	return b.String()
}

type appliedRow struct{ Checksum, ModelHash string }

func applied(ctx context.Context, conn *pgx.Conn) (map[string]appliedRow, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum, model_hash FROM ks_migrations`)
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

type statusRow struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	State   string `json:"state"` // applied|pending|modified
}

func status(ctx context.Context, conn *pgx.Conn) ([]statusRow, []migration, error) {
	all, err := migrations()
	if err != nil {
		return nil, nil, err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	var out []statusRow
	for _, m := range all {
		r := statusRow{Version: m.Version, Name: m.Name, State: "pending"}
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

func migrateStatus(ctx context.Context, url string, asJSON bool) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	rows, _, err := status(ctx, conn)
	if err != nil {
		return err
	}
	if asJSON {
		printJSON(rows)
	} else {
		for _, r := range rows {
			fmt.Printf("%s_%s  %s\n", r.Version, r.Name, r.State)
		}
	}
	for _, r := range rows {
		if r.State == "modified" {
			return modified(r)
		}
	}
	return nil
}

func modified(r statusRow) error {
	return fmt.Errorf("KS-MIG-002: applied migration %s_%s was edited; applied migrations are immutable, plan a new one instead", r.Version, r.Name)
}

func migrateApply(ctx context.Context, url string, allowed map[string]bool, asJSON bool) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	rows, all, err := status(ctx, conn)
	if err != nil {
		return err
	}
	var ran []string
	for i, r := range rows {
		switch r.State {
		case "modified":
			return modified(r)
		case "applied":
			continue
		}
		m := all[i]
		// Every flag the plan asks for must be given before anything runs (KS-MIG-001).
		for _, s := range m.Plan.Steps {
			if s.NeedsFlag != "" && !allowed[s.NeedsFlag] {
				return fmt.Errorf("KS-MIG-001: %s_%s step %q (%s) needs %s", m.Version, m.Name, s.Detail, s.Class, s.NeedsFlag)
			}
			if s.Backfill != nil && strings.TrimSpace(s.Backfill.Set) == "" {
				return fmt.Errorf("%s_%s step %q backfills %s.%s: write the value each existing row gets into the plan's backfill.set",
					m.Version, m.Name, s.Detail, s.Backfill.Table, s.Backfill.Column)
			}
		}
		if err := applyOne(ctx, conn, m, allowed["--allow-destructive"]); err != nil {
			return fmt.Errorf("%s_%s: %w", m.Version, m.Name, err)
		}
		ran = append(ran, m.Version+"_"+m.Name)
	}
	if asJSON {
		printJSON(map[string]any{"applied": ran})
	} else if len(ran) == 0 {
		fmt.Println("up to date")
	} else {
		for _, r := range ran {
			fmt.Println("applied", r)
		}
	}
	return nil
}

// applyOne runs a plan's transactional statements under lock and statement timeouts,
// retrying lock timeouts with backoff, then the statements that cannot run in a
// transaction (§13.1 step 4).
func applyOne(ctx context.Context, conn *pgx.Conn, m migration, destructive bool) error {
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
		`INSERT INTO ks_migrations (version, name, checksum, model_hash, destructive_allowed) VALUES ($1, $2, $3, $4, $5)`,
		m.Version, m.Name, m.Checksum, m.Plan.ModelHash, destructive)
	return err
}

// backfill fills a column's nulls in batches keyed by primary key, one transaction per
// batch, recording the cursor in ks_backfills so an interrupted run resumes where it
// stopped rather than starting over (§13.3).
func backfill(ctx context.Context, conn *pgx.Conn, version string, b migrate.Backfill) error {
	if b.Batch <= 0 {
		b.Batch = 1000
	}
	// The migration role writes every tenant's rows.
	if _, err := conn.Exec(ctx, "SELECT set_config('ks.cross_tenant', 'on', false)"); err != nil {
		return err
	}
	var cursor string
	err := conn.QueryRow(ctx, `INSERT INTO ks_backfills (id, version, column_name, cursor) VALUES ($1, $2, $3, '""')
		ON CONFLICT (version, column_name) DO UPDATE SET updated_at = now()
		RETURNING coalesce(cursor #>> '{}', '')`, ks.NewID(), version, b.Table+"."+b.Column).Scan(&cursor)
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
			_, err = tx.Exec(ctx, `UPDATE ks_backfills SET cursor = to_jsonb($3::text), rows_done = rows_done + $4, state = 'running', updated_at = now()
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
	_, err = conn.Exec(ctx, `UPDATE ks_backfills SET state = 'done', updated_at = now() WHERE version = $1 AND column_name = $2`,
		version, b.Table+"."+b.Column)
	return err
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// migrateVerify applies every migration to a fresh database and checks the result has
// the schema of the current model: a plan that drifts from the model fails here, not in
// production (§13.1 step 3).
func migrateVerify(ctx context.Context, url string, asJSON bool) error {
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	// scratch creates an empty database and connects to it; close drops it again.
	scratch := func() (*pgx.Conn, func(), error) {
		name := "ks_verify_" + strings.ReplaceAll(string(ks.NewID()), "-", "")
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			return nil, nil, err
		}
		drop := func() { admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") }
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			drop()
			return nil, nil, err
		}
		cfg.Database = name
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			drop()
			return nil, nil, err
		}
		return c, func() { c.Close(ctx); drop() }, nil
	}
	a, closeA, err := scratch()
	if err != nil {
		return err
	}
	defer closeA()
	b, closeB, err := scratch()
	if err != nil {
		return err
	}
	defer closeB()

	all, err := migrations()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("no migrations; run ks migrate plan")
	}
	for _, m := range all {
		if err := applyOne(ctx, a, m, true); err != nil {
			return fmt.Errorf("%s_%s does not apply to a fresh database: %w", m.Version, m.Name, err)
		}
	}
	head, diags, err := loadModel()
	if err != nil {
		return err
	}
	if hasErrors(diags) {
		return report(diags, asJSON, false)
	}
	for _, stmt := range migrate.DDL(head) {
		if _, err := b.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("the model's schema does not apply: %w", err)
		}
	}
	got, err := schemaShape(ctx, a)
	if err != nil {
		return err
	}
	want, err := schemaShape(ctx, b)
	if err != nil {
		return err
	}
	var drift []string
	for k := range want {
		if !got[k] {
			drift = append(drift, "missing after migrations: "+k)
		}
	}
	for k := range got {
		if !want[k] {
			drift = append(drift, "not in the model: "+k)
		}
	}
	sort.Strings(drift)
	if asJSON {
		printJSON(map[string]any{"verified": len(drift) == 0, "migrations": len(all), "drift": drift})
	} else if len(drift) == 0 {
		fmt.Printf("%d migrations apply to a fresh database and match the model\n", len(all))
	} else {
		for _, d := range drift {
			fmt.Println(d)
		}
	}
	if len(drift) > 0 {
		return exitError{1}
	}
	return nil
}

// schemaShape is a comparable summary of the public schema: columns with type and
// nullability, constraints and indexes by name.
func schemaShape(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || CASE WHEN is_nullable = 'NO' THEN ' not null' ELSE '' END
		  FROM information_schema.columns WHERE table_schema = 'public'
		UNION ALL
		SELECT 'constraint ' || conrelid::regclass::text || '.' || conname FROM pg_constraint
		 WHERE connamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'index ' || tablename || '.' || indexname FROM pg_indexes WHERE schemaname = 'public'
		UNION ALL
		SELECT 'policy ' || tablename || '.' || policyname FROM pg_policies WHERE schemaname = 'public'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}
