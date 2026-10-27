package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Operating the outbox and the job queue (§10.1, §10.4):
//
//	ks outbox list [--state pending|dead_letter|done] [--limit n]
//	ks outbox inspect <id>
//	ks outbox replay <id>      put a dead-lettered or failed effect back in the queue
//	ks jobs list|inspect|replay  the same for jobs
//
// Replay resets attempts and makes the row runnable now; the idempotency key is kept, so
// a provider that saw the first attempt recognises the retry.
type queue struct {
	table, live, columns string
}

var queues = map[string]queue{
	"outbox": {"ks_outbox", "pending",
		"id::text, coalesce(tenant_id::text,''), action, type, state, attempts, coalesce(last_error,''), created_at, run_after, payload::text"},
	"jobs": {"ks_jobs", "queued",
		"id::text, coalesce(tenant_id::text,''), action, 'job', state, attempts, coalesce(last_error,''), created_at, run_after, args::text"},
}

type queueRow struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenantId,omitempty"`
	Action    string    `json:"action"`
	Type      string    `json:"type"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"lastError,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	RunAfter  time.Time `json:"runAfter"`
	Payload   string    `json:"payload,omitempty"`
}

func cmdOutbox(args []string) error { return cmdQueue("outbox", args) }
func cmdJobs(args []string) error   { return cmdQueue("jobs", args) }

func cmdQueue(name string, args []string) error {
	q := queues[name]
	var state *string
	var limit *int
	fs, asJSON, err := flags(name, args, func(fs *flag.FlagSet) {
		state = fs.String("state", "", "list: only rows in this state")
		limit = fs.Int("limit", 50, "list: at most this many rows")
	})
	if err != nil {
		return err
	}
	url, err := databaseURL("KS_DATABASE_URL")
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// An operator works across tenants.
	if _, err := conn.Exec(ctx, "SELECT set_config('ks.cross_tenant', 'on', false), set_config('ks.actor_system', 'on', false)"); err != nil {
		return err
	}
	scan := func(rows pgx.Rows) ([]queueRow, error) {
		defer rows.Close()
		var out []queueRow
		for rows.Next() {
			var r queueRow
			if err := rows.Scan(&r.ID, &r.Tenant, &r.Action, &r.Type, &r.State, &r.Attempts, &r.LastError, &r.CreatedAt, &r.RunAfter, &r.Payload); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	switch fs.Arg(0) {
	case "list":
		where, params := "", []any{*limit}
		if *state != "" {
			where, params = "WHERE state = $2", append(params, *state)
		}
		rows, err := conn.Query(ctx, fmt.Sprintf("SELECT %s FROM %s %s ORDER BY created_at DESC LIMIT $1", q.columns, q.table, where), params...)
		if err != nil {
			return err
		}
		list, err := scan(rows)
		if err != nil {
			return err
		}
		if asJSON {
			if list == nil {
				list = []queueRow{}
			}
			printJSON(list)
			return nil
		}
		for _, r := range list {
			fmt.Printf("%s  %-11s %-8s %-28s attempts=%d  %s\n", r.ID, r.State, r.Type, r.Action, r.Attempts, oneLine(r.LastError))
		}
		return nil
	case "inspect", "replay":
		id := fs.Arg(1)
		if id == "" {
			return fmt.Errorf("usage: ks %s %s <id>", name, fs.Arg(0))
		}
		if fs.Arg(0) == "replay" {
			tag, err := conn.Exec(ctx, fmt.Sprintf(`UPDATE %s SET state = $2, attempts = 0, run_after = now(), last_error = NULL,
				claimed_by = NULL, claimed_at = NULL WHERE id = $1::uuid AND state <> $2 AND state <> 'running'`, q.table), id, q.live)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%s %s is not replayable (missing, already %s, or running)", name, id, q.live)
			}
			if name == "outbox" {
				_, _ = conn.Exec(ctx, "SELECT pg_notify('ks_outbox', '')")
			}
		}
		rows, err := conn.Query(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE id = $1::uuid", q.columns, q.table), id)
		if err != nil {
			return err
		}
		list, err := scan(rows)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return fmt.Errorf("no %s row %s", name, id)
		}
		if asJSON {
			printJSON(list[0])
			return nil
		}
		raw, _ := json.MarshalIndent(list[0], "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	return fmt.Errorf("usage: ks %s list|inspect|replay", name)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
