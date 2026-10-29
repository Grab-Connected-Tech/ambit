package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// rawTx is the am.RawTx handed to am.Unsafe (§8.6). Raw SQL skips the query layer, so
// each statement is held to what can still be enforced around it: one statement per
// call, no rewriting the session's security settings, the action's budget, and rows
// handed back only after the settings are verified unchanged.
type rawTx struct {
	d    *DB
	want string // the security settings before the first raw statement
}

// rawSettings is everything RLS and the timeouts depend on. A raw statement that
// changes any of it has stepped outside the action's scope.
const rawSettings = `SELECT ARRAY[
	coalesce(current_setting('am.tenant_id', true), ''),
	coalesce(current_setting('am.actor_id', true), ''),
	coalesce(current_setting('am.actor_system', true), ''),
	coalesce(current_setting('am.cross_tenant', true), ''),
	current_user::text, session_user::text,
	current_setting('statement_timeout'), current_setting('lock_timeout')]::text`

var rawFirstWord = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true,
	"with": true, "values": true, "table": true, "merge": true,
}

// rawDenied names the built-ins that set a session setting or execute SQL held in a
// string, which would let a statement rewrite the scope and read under it in one go.
// ponytail: a deny-list of known built-ins; a user-defined function that does the
// same is out of its reach, which is why the settings are re-read after every
// statement and a change refuses the commit.
var rawDenied = regexp.MustCompile(`\b(set_config|query_to_xml\w*|cursor_to_xml\w*|ts_stat|ts_rewrite|dblink\w*)\b|\bu&`)

// checkRawSQL refuses a statement that is not plain DML or a read.
func checkRawSQL(sql string) error {
	s := strings.ToLower(sql)
	first := strings.TrimLeft(s, " \t\r\n(")
	if i := strings.IndexFunc(first, func(r rune) bool { return r < 'a' || r > 'z' }); i >= 0 {
		first = first[:i]
	}
	if !rawFirstWord[first] {
		return am.Invalid("sql", "am.Unsafe runs SELECT, INSERT, UPDATE, DELETE, MERGE, WITH, VALUES or TABLE statements only, starting with the keyword")
	}
	if m := rawDenied.FindString(s); m != "" {
		return am.Invalid("sql", fmt.Sprintf("am.Unsafe may not use %s: it can change the transaction's security settings or run SQL from a string; pass values as parameters", m))
	}
	return nil
}

// checkRawArgs refuses pgx query options smuggled in as arguments: one could switch
// the statement to the simple protocol, where a single string may hold several.
func checkRawArgs(args []any) error {
	for _, a := range args {
		switch a.(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID, pgx.QueryRewriter:
			return am.Invalid("args", "am.Unsafe arguments must be values, not query options")
		}
	}
	return nil
}

func (r *rawTx) settings(ctx context.Context) (string, error) {
	var s string
	if err := r.d.tx.QueryRow(ctx, rawSettings).Scan(&s); err != nil {
		return "", translate(err)
	}
	return s, nil
}

// run executes one statement and buffers its rows, so nothing reaches the action
// until the settings check has passed.
func (r *rawTx) run(ctx context.Context, sql string, args []any) (*rawRows, int64, error) {
	if err := r.d.usable(); err != nil {
		return nil, 0, err
	}
	if err := checkRawSQL(sql); err != nil {
		return nil, 0, err
	}
	if err := checkRawArgs(args); err != nil {
		return nil, 0, err
	}
	if r.want == "" {
		s, err := r.settings(ctx)
		if err != nil {
			return nil, 0, err
		}
		r.want = s
	}
	start := time.Now()
	// The extended protocol takes exactly one statement, so a stacked "; ..." is
	// refused by Postgres itself. pgx would pick the simple protocol for a call
	// without arguments unless told otherwise.
	rows, err := r.d.tx.Query(ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		r.d.record(sql, args, 0, start)
		return nil, 0, translate(err)
	}
	limit := r.d.cfg.RowBudget
	if r.d.budget != nil && r.d.budget.MaxR > 0 {
		limit = r.d.budget.MaxR
	}
	out := &rawRows{m: r.d.tx.Conn().TypeMap(), fds: slices.Clone(rows.FieldDescriptions())}
	for rows.Next() {
		if limit > 0 && int64(len(out.rows)) >= limit {
			rows.Close()
			r.d.record(sql, args, int64(len(out.rows)), start)
			return nil, 0, am.BudgetExceeded("row", int(limit))
		}
		raw := rows.RawValues()
		row := make([][]byte, len(raw))
		for i, v := range raw {
			row[i] = bytes.Clone(v)
		}
		out.rows = append(out.rows, row)
	}
	rows.Close()
	affected := rows.CommandTag().RowsAffected()
	counted := affected
	if n := int64(len(out.rows)); n > counted {
		counted = n
	}
	r.d.record(sql, args, counted, start)
	if err := rows.Err(); err != nil {
		return nil, 0, translate(err)
	}
	got, err := r.settings(ctx)
	if err != nil {
		return nil, 0, err
	}
	if got != r.want {
		err := am.InternalError(errors.New("am.Unsafe: raw SQL changed the transaction's security settings; the transaction will not commit"))
		r.d.tainted = err
		return nil, 0, err
	}
	return out, affected, r.d.budgetErr()
}

func (r *rawTx) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	_, n, err := r.run(ctx, sql, args)
	return n, err
}

func (r *rawTx) Query(ctx context.Context, sql string, args ...any) (am.Rows, error) {
	rows, _, err := r.run(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ExecOne runs a statement that must touch exactly one row; none is not_found.
func (r *rawTx) ExecOne(ctx context.Context, sql string, args ...any) error {
	n, err := r.Exec(ctx, sql, args...)
	switch {
	case err != nil:
		return err
	case n == 0:
		return am.NotFound()
	case n > 1:
		return am.InternalError(fmt.Errorf("am.Unsafe: ExecOne touched %d rows", n))
	}
	return nil
}

// rawRows is a fully read result, decoded on Scan with the connection's type map.
type rawRows struct {
	m    *pgtype.Map
	fds  []pgconn.FieldDescription
	rows [][][]byte
	i    int
}

func (r *rawRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *rawRows) Scan(dest ...any) error {
	if r.i == 0 {
		return errors.New("am: Scan called before Next")
	}
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("am: %d scan targets for %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		if err := r.m.Scan(r.fds[i].DataTypeOID, r.fds[i].Format, row[i], d); err != nil {
			return fmt.Errorf("am: column %s: %w", r.fds[i].Name, err)
		}
	}
	return nil
}

func (r *rawRows) Columns() ([]string, error) {
	out := make([]string, len(r.fds))
	for i, fd := range r.fds {
		out[i] = fd.Name
	}
	return out, nil
}

func (r *rawRows) Err() error { return nil }
func (r *rawRows) Close()     {}
