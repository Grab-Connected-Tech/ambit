package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Grab-Connected-Tech/ambit/am"
)

// A database refusal must say which constraint refused it; a unique violation reported
// as "changed by someone else" sent an agent hunting for a concurrent writer.
func TestTranslateNamesTheConstraint(t *testing.T) {
	var e *am.Error
	errors.As(translate(&pgconn.PgError{Code: "23505", ConstraintName: "user_account_email_key"}), &e)
	if e == nil || e.Code != am.CodeConflict || !strings.Contains(e.Message, "user_account_email_key") || e.Message == am.Conflict().Message {
		t.Errorf("unique violation: %+v", e)
	}
	e = nil
	errors.As(translate(&pgconn.PgError{Code: "23514", ConstraintName: "expense_amount_positive"}), &e)
	if e == nil || e.Code != am.CodeInvariant || !strings.Contains(e.Message, "expense_amount_positive") {
		t.Errorf("check violation: %+v", e)
	}
}
