package migrate

import (
	"fmt"
	"strings"

	"keystone.dev/keystone/model"
)

// FrameworkDDL writes the tables the runtime itself needs. They are all prefixed
// ks_ so they never collide with an app's tables, and they are part of the same
// generated migration stream rather than a hidden bootstrap (§2.4: the code that
// runs is the code you see).
// ensureFrameworkSteps adds the framework tables that a plan is missing. They are
// created idempotently because an existing install may already have them.
func ensureFrameworkSteps(a *model.App) []Step {
	stmts := FrameworkDDL(a)
	if len(stmts) == 0 {
		return nil
	}
	return []Step{{
		Class:   Safe,
		Subject: "ks framework tables",
		Detail:  "ensure the runtime's own tables exist",
		InTx:    withIFNotExists(stmts),
	}}
}

// withIFNotExists turns the framework's DROP+CREATE script into an additive one,
// which is what an upgrade needs.
func withIFNotExists(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.Replace(s, "DROP TABLE IF EXISTS", "--", 1)
		s = strings.Replace(s, "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ", 1)
		out = append(out, s)
	}
	return out
}

func FrameworkDDL(a *model.App) []string {
	tenant := "org"
	if a != nil && a.Tenant != nil {
		tenant = a.Tenant.Table
	}
	out := []string{
		`CREATE OR REPLACE FUNCTION ks_guard_immutable() RETURNS trigger AS $$
DECLARE c text; old_v text; new_v text;
BEGIN
  FOREACH c IN ARRAY TG_ARGV LOOP
    EXECUTE format('SELECT ($1).%I::text', c) INTO old_v USING OLD;
    EXECUTE format('SELECT ($1).%I::text', c) INTO new_v USING NEW;
    IF old_v IS DISTINCT FROM new_v THEN
      RAISE EXCEPTION 'column % is immutable', c USING ERRCODE = '23514';
    END IF;
  END LOOP;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;`,
		`CREATE OR REPLACE FUNCTION ks_guard_append_only() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'this table is append-only' USING ERRCODE = '42501';
END;
$$ LANGUAGE plpgsql;`,
		`CREATE OR REPLACE FUNCTION ks_touch_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;`}
	tables := []struct {
		name   string
		body   string
		tenant bool
	}{
		{"ks_membership", fmt.Sprintf(`  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  role text NOT NULL,
  granted_by uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_membership_unique UNIQUE (tenant_id, actor_id, role)`, quoteIdent(tenant)), true},
		{"ks_session", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  actor_kind text NOT NULL DEFAULT 'user',
  token_hash bytea NOT NULL,
  rotate_from uuid,
  user_agent text,
  ip inet,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  idle_expires_at timestamptz,
  revoked_at timestamptz,
  CONSTRAINT ks_session_token UNIQUE (token_hash)`, false},
		{"ks_api_token", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  name text NOT NULL,
  token_hash bytea NOT NULL,
  scopes text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at timestamptz,
  CONSTRAINT ks_api_token_hash UNIQUE (token_hash)`, false},
		{"ks_identity_link", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  provider text NOT NULL,
  subject text NOT NULL,
  email text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_identity_link_unique UNIQUE (provider, subject)`, false},
		{"ks_credential", `  id uuid NOT NULL PRIMARY KEY,
  actor_id uuid NOT NULL,
  algo text NOT NULL,
  hash bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  failed_attempts integer NOT NULL DEFAULT 0,
  locked_until timestamptz,
  CONSTRAINT ks_credential_actor UNIQUE (actor_id)`, false},
		{"ks_outbox", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  action text NOT NULL,
  invocation_id uuid,
  seq integer NOT NULL,
  type text NOT NULL,
  mode text NOT NULL DEFAULT 'deferred',
  template text,
  topic text,
  bucket text,
  hosts text[],
  idempotency_key text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  state text NOT NULL DEFAULT 'pending',
  attempts integer NOT NULL DEFAULT 0,
  run_after timestamptz NOT NULL DEFAULT now(),
  claimed_by text,
  claimed_at timestamptz,
  last_error text,
  result jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  dead_lettered_at timestamptz,
  CONSTRAINT ks_outbox_idem UNIQUE (idempotency_key),
  CONSTRAINT ks_outbox_state CHECK (state IN ('pending','running','done','dead_letter'))`, false},
		{"ks_jobs", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  action text NOT NULL,
  args jsonb NOT NULL DEFAULT '{}',
  priority integer NOT NULL DEFAULT 5,
  state text NOT NULL DEFAULT 'queued',
  run_after timestamptz NOT NULL DEFAULT now(),
  attempts integer NOT NULL DEFAULT 0,
  max_attempts integer NOT NULL DEFAULT 5,
  backoff text NOT NULL DEFAULT 'exponential',
  base_delay interval NOT NULL DEFAULT '1 second',
  cap_delay interval NOT NULL DEFAULT '10 minutes',
  concurrency_key text,
  claimed_by text,
  claimed_at timestamptz,
  heartbeat_at timestamptz,
  last_error text,
  invocation_id uuid,
  policy jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  CONSTRAINT ks_jobs_priority CHECK (priority BETWEEN 0 AND 9),
  CONSTRAINT ks_jobs_state CHECK (state IN ('queued','running','done','dead_letter','canceled'))`, false},
		{"ks_scheduled_runs", `  id uuid NOT NULL PRIMARY KEY,
  schedule text NOT NULL,
  slot text NOT NULL,
  tenant_id uuid,
  job_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_scheduled_run_unique UNIQUE (schedule, slot, tenant_id)`, false},
		{"ks_invocations", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  actor_id uuid,
  actor_kind text NOT NULL DEFAULT 'user',
  actor_roles text[] NOT NULL DEFAULT '{}',
  action text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()`, false},
		{"ks_audit", `  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id uuid,
  actor_id uuid,
  actor_kind text NOT NULL,
  action text NOT NULL,
  resource text,
  resource_id uuid,
  changed_fields text[],
  value_before jsonb,
  value_after jsonb,
  trace_id text,
  created_at timestamptz NOT NULL DEFAULT now()`, true},
		{"ks_migrations", `  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  version text NOT NULL UNIQUE,
  name text NOT NULL,
  checksum text NOT NULL,
  model_hash text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now(),
  destructive_allowed boolean NOT NULL DEFAULT false`, false},
		{"ks_backfills", `  id uuid NOT NULL PRIMARY KEY,
  version text NOT NULL,
  column_name text NOT NULL,
  cursor jsonb,
  rows_done bigint NOT NULL DEFAULT 0,
  total_estimated bigint,
  state text NOT NULL DEFAULT 'pending',
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_backfill_unique UNIQUE (version, column_name)`, false},
		{"ks_idempotency", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  action text NOT NULL,
  key text NOT NULL,
  request_hash bytea NOT NULL,
  response text,
  status_code integer,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  CONSTRAINT ks_idempotency_unique UNIQUE (action, tenant_id, key)`, false},
		{"ks_rate_bucket", `  id uuid NOT NULL PRIMARY KEY,
  scope text NOT NULL,
  bucket_key text NOT NULL,
  action text NOT NULL,
  tokens double precision NOT NULL,
  max_tokens double precision NOT NULL,
  refill_seconds double precision NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_rate_bucket_unique UNIQUE (action, scope, bucket_key)`, false},
		{"ks_cache_entry", `  id uuid NOT NULL PRIMARY KEY,
  cache_key text NOT NULL UNIQUE,
  tenant_id uuid,
  value jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  generation bigint NOT NULL DEFAULT 0`, false},
		{"ks_cache_generation", `  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL,
  resource text NOT NULL,
  generation bigint NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_cache_generation_unique UNIQUE (tenant_id, resource)`, false},
	}
	for _, t := range tables {
		if t.name == "ks_credential" && (a == nil || a.Identity.Passwords == nil) {
			continue
		}
		if t.name == "ks_identity_link" && (a == nil || len(a.Identity.OIDC) == 0) {
			continue
		}
		body := t.body
		if strings.Contains(body, "%s") {
			body = fmt.Sprintf(body, quoteIdent(tenant))
		}
		out = append(out, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE;\nCREATE TABLE %s (\n%s\n);", quoteIdent(t.name), quoteIdent(t.name), body))
		res := model.Resource{Name: t.name, Table: t.name, Tenant: t.tenant}
		out = append(out, rlsof(res)...)
	}
	out = append(out,
		"CREATE INDEX IF NOT EXISTS ks_outbox_claim ON ks_outbox (state, run_after) WHERE state = 'pending';",
		"CREATE INDEX IF NOT EXISTS ks_jobs_claim ON ks_jobs (priority, run_after) WHERE state = 'queued';",
		"CREATE INDEX IF NOT EXISTS ks_session_actor ON ks_session (actor_id);",
		"CREATE INDEX IF NOT EXISTS ks_audit_tenant_time ON ks_audit (tenant_id, created_at);",
		// Audit records are append-only: the database refuses edits even to the owner.
		"CREATE TRIGGER ks_audit_guard BEFORE UPDATE OR DELETE ON ks_audit FOR EACH ROW EXECUTE FUNCTION ks_guard_append_only();",
		"REVOKE UPDATE, DELETE, TRUNCATE ON ks_audit FROM PUBLIC;",
	)
	return out
}
