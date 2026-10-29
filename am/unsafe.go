package am

import (
	"context"
	"errors"
)

// Rows is the minimal row-set view handed to raw SQL blocks. It intentionally
// exposes no driver types: the runtime wraps pgx rows and counts them.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Err() error
	Close()
}

// RawTx is the transaction handle available inside am.Unsafe (§8.6). Calls still
// carry the tenant settings used by row-level security and count against the
// action's query budget.
type RawTx interface {
	Exec(ctx context.Context, sql string, args ...any) (rowsAffected int64, err error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	ExecOne(ctx context.Context, sql string, args ...any) error
}

type rawTxKey struct{}

// WithRawTx attaches the raw-SQL runner used by am.Unsafe. The runtime calls it;
// app code never does.
func WithRawTx(ctx context.Context, tx RawTx) context.Context {
	return context.WithValue(ctx, rawTxKey{}, tx)
}

// Unsafe runs a raw-SQL block, marking the weakened guarantee for the behavior
// diff. The reason is mandatory: an empty reason fails the checker (AM-SEC-003).
func Unsafe(ctx context.Context, reason string, fn func(RawTx) error) error {
	if reason == "" {
		return Invalid("reason", "am.Unsafe requires a non-empty reason (AM-SEC-003)")
	}
	tx, ok := ctx.Value(rawTxKey{}).(RawTx)
	if !ok {
		return InternalError(errors.New("am: am.Unsafe called outside an action transaction"))
	}
	return fn(tx)
}

// UnsafeBlock records one raw-SQL escape hatch, extracted from source by the
// checker into app.facts.json (§6.3).
type UnsafeBlock struct {
	File   string
	Line   int
	Module string
	Action string
	Reason string
}
