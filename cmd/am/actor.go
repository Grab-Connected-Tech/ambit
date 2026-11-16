package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Grab-Connected-Tech/ambit/am"
	"github.com/Grab-Connected-Tech/ambit/model"
	"github.com/Grab-Connected-Tech/ambit/runtime"
)

// pairs is a repeatable column=value flag.
type pairs map[string]string

func (p pairs) String() string { return "" }
func (p pairs) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want column=value, got %q", s)
	}
	p[k] = v
	return nil
}

// list is a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

// cmdActor creates what a person needs to call the app: a tenant (or an existing one),
// an account, its roles and an API token, in one transaction against AM_DATABASE_URL.
// The token is printed once; only its hash is stored.
func cmdActor(args []string) error {
	err := actorCreate(args)
	// A CLI user is the operator: show what the database said, not the client message.
	var e *am.Error
	if errors.As(err, &e) && e.Internal != nil {
		return fmt.Errorf("%s: %w", e.Message, e.Internal)
	}
	return err
}

func actorCreate(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: am actor create [--tenant <id> | --tenant-field col=value ...] --field col=value ... --role <role> ... [--token-name name]")
	}
	tenantFields, accountFields := pairs{}, pairs{}
	var roles list
	var tenantID, tokenName *string
	_, asJSON, err := flags("actor create", args[1:], func(fs *flag.FlagSet) {
		tenantID = fs.String("tenant", "", "an existing tenant id; omit to create a tenant")
		fs.Var(tenantFields, "tenant-field", "a column of the new tenant, e.g. name=Acme (repeatable)")
		fs.Var(accountFields, "field", "a column of the account, e.g. email=mia@acme.test (repeatable)")
		fs.Var(&roles, "role", "a role to grant in the tenant (repeatable)")
		tokenName = fs.String("token-name", "cli", "a label for the API token")
	})
	if err != nil {
		return err
	}
	raw, err := os.ReadFile("app.model.json")
	if err != nil {
		return fmt.Errorf("app.model.json: %w (run am gen in the app directory)", err)
	}
	app, err := model.Load(raw)
	if err != nil {
		return err
	}
	if app.Tenant == nil || app.Account == nil {
		return errors.New("the app declares no tenant or account resource")
	}
	if len(roles) == 0 {
		return errors.New("grant at least one --role; an actor with none can call nothing")
	}
	for _, r := range roles {
		if !slices.Contains(app.Roles, r) {
			return fmt.Errorf("the app declares no role %q (it has %s)", r, strings.Join(app.Roles, ", "))
		}
	}
	url, err := databaseURL("AM_DATABASE_URL")
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('am.cross_tenant', 'on', true), set_config('am.actor_system', 'on', true)"); err != nil {
		return err
	}

	tenant := am.ID(*tenantID)
	if tenant.IsNil() {
		tenant = am.NewID()
		if err := insertRow(ctx, tx, app.Tenant, tenant, tenant, tenantFields); err != nil {
			return fmt.Errorf("tenant: %w", err)
		}
	} else if len(tenantFields) > 0 {
		return errors.New("--tenant-field only applies when creating a tenant; drop --tenant to create one")
	}
	actor := am.NewID()
	if err := insertRow(ctx, tx, app.Account, actor, tenant, accountFields); err != nil {
		return fmt.Errorf("account: %w", err)
	}
	for _, r := range roles {
		if err := runtime.GrantRoleTx(ctx, tx, app, tenant, actor, r); err != nil {
			return fmt.Errorf("role %s: %w", r, err)
		}
	}
	token, err := runtime.IssueAPIToken(ctx, tx, tenant, actor, *tokenName, nil)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	out := map[string]any{"tenant": tenant, "actor": actor, "roles": roles, "token": token}
	if asJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("tenant %s\nactor  %s (%s)\ntoken  %s\n\nShown once. Send it as: Authorization: Bearer %s\n",
		tenant, actor, strings.Join(roles, ", "), token, token)
	return nil
}

// insertRow writes one tenant or account row from column=value pairs. Columns are
// checked against the model, so a typo is an error rather than SQL, and values go in as
// parameters for Postgres to convert to each column's type.
func insertRow(ctx context.Context, tx pgx.Tx, res *model.Tenant, id, tenant am.ID, set pairs) error {
	cols := []string{"id"}
	args := []any{id}
	known := map[string]bool{}
	for _, f := range res.Fields {
		known[f.Column] = true
	}
	if known["tenant_id"] {
		cols, args = append(cols, "tenant_id"), append(args, tenant)
	}
	for c, v := range set {
		switch {
		case !known[c]:
			return fmt.Errorf("%s has no column %q", res.Table, c)
		case c == "id" || c == "tenant_id" || c == "created_at" || c == "updated_at" || c == "version":
			return fmt.Errorf("%s is set by the framework", c)
		}
		cols, args = append(cols, c), append(args, v)
	}
	holes := make([]string, len(cols))
	quoted := make([]string, len(cols))
	for i, c := range cols {
		holes[i] = fmt.Sprintf("$%d", i+1)
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	_, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		pgx.Identifier{res.Table}.Sanitize(), strings.Join(quoted, ", "), strings.Join(holes, ", ")), args...)
	return err
}
