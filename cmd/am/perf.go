package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Grab-Connected-Tech/ambit/compile"
)

// cmdPerf explains the query shapes am test captured against a seeded database and flags
// sequential scans on tables above a size (AM-PERF-002, §11.4), naming the filtered
// columns as the index to declare.
func cmdPerf(args []string) error {
	var minRows *int64
	_, asJSON, err := flags("perf", args, func(fs *flag.FlagSet) {
		minRows = fs.Int64("min-rows", 1000, "only flag tables with at least this many rows")
	})
	if err != nil {
		return err
	}
	url, err := databaseURL("AM_DATABASE_URL")
	if err != nil {
		return err
	}
	shapes, err := readShapes(filepath.Join(".am", "queries.jsonl"))
	if err != nil {
		return fmt.Errorf("%w (run am test first: it captures the query shapes)", err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	d, skipped := explainShapes(ctx, conn, shapes, *minRows)
	if skipped > 0 && !asJSON {
		fmt.Fprintf(os.Stderr, "am perf: %d shapes could not be explained with their captured arguments\n", skipped)
	}
	return report(d, asJSON, false)
}

type shape struct {
	Action string   `json:"action"`
	SQL    string   `json:"sql"`
	Args   []string `json:"args"`
}

func readShapes(path string) ([]shape, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []shape
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var s shape
		if json.Unmarshal(sc.Bytes(), &s) != nil || seen[s.Action+"\x00"+s.SQL] {
			continue
		}
		seen[s.Action+"\x00"+s.SQL] = true
		out = append(out, s)
	}
	return out, sc.Err()
}

var filterColumn = regexp.MustCompile(`\(?(?:[a-z_]+\.)?([a-z_][a-z0-9_]*) (?:=|<|>|<=|>=|~~|IS)`)

func explainShapes(ctx context.Context, conn *pgx.Conn, shapes []shape, minRows int64) (compile.Diags, int) {
	var d compile.Diags
	skipped := 0
	flagged := map[string]bool{}
	for _, s := range shapes {
		if !strings.HasPrefix(strings.TrimSpace(strings.ToUpper(s.SQL)), "SELECT") {
			continue // writes are explained through the reads they make
		}
		params := make([]any, len(s.Args))
		for i, a := range s.Args {
			params[i] = a
		}
		var plan []byte
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// The plan an operator sees: every tenant's rows, no session predicate.
			if _, err := tx.Exec(ctx, "SELECT set_config('am.cross_tenant','on',true), set_config('am.actor_system','on',true)"); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+s.SQL, params...).Scan(&plan)
		})
		if err != nil {
			skipped++
			continue
		}
		var doc []struct {
			Plan planNode `json:"Plan"`
		}
		if json.Unmarshal(plan, &doc) != nil || len(doc) == 0 {
			skipped++
			continue
		}
		for _, n := range doc[0].Plan.seqScans() {
			var rows float64
			if err := conn.QueryRow(ctx, "SELECT reltuples FROM pg_class WHERE relname = $1", n.Relation).Scan(&rows); err != nil || int64(rows) < minRows {
				continue
			}
			key := s.Action + "|" + n.Relation
			if flagged[key] {
				continue
			}
			flagged[key] = true
			cols := filterColumns(n.Filter)
			fix := "add an index covering the filtered columns to the resource's spec"
			if len(cols) > 0 {
				fix = fmt.Sprintf("add s.Index(%s) to the resource on table %s", quoteList(cols), n.Relation)
			}
			d = append(d, compile.Diag{Rule: "AM-PERF-002", Severity: compile.SevWarning, Decl: s.Action,
				Message: fmt.Sprintf("plans a sequential scan on %s (%.0f rows): %s", n.Relation, rows, oneLine(n.Filter)),
				Fix:     fix, Docs: "am explain AM-PERF-002"})
		}
	}
	return d, skipped
}

type planNode struct {
	Type     string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Filter   string     `json:"Filter"`
	Plans    []planNode `json:"Plans"`
}

func (n planNode) seqScans() []planNode {
	var out []planNode
	if n.Type == "Seq Scan" && !strings.HasPrefix(n.Relation, "am_") {
		out = append(out, n)
	}
	for _, k := range n.Plans {
		out = append(out, k.seqScans()...)
	}
	return out
}

// filterColumns picks the column names a scan filter compares, which are the index
// candidates. Framework columns that every row has are left out.
func filterColumns(filter string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range filterColumn.FindAllStringSubmatch(filter, -1) {
		c := m[1]
		if seen[c] || c == "tenant_id" || c == "current_setting" || c == "coalesce" {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = fmt.Sprintf("%q", c)
	}
	return strings.Join(q, ", ")
}
