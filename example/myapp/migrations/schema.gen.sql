-- generated from the app model by ks gen; do not edit

CREATE OR REPLACE FUNCTION ks_new_id() RETURNS uuid AS $$
  SELECT gen_random_uuid()
$$ LANGUAGE sql VOLATILE;

CREATE TABLE "org" (
  "tenant_id" uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  "id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "name" text NOT NULL CHECK (length("name") <= 200),
  "plan" text NOT NULL DEFAULT 'free' CHECK ("plan" IN ('enterprise','free','pro')),
  CONSTRAINT "org_pk" PRIMARY KEY (id)
);

COMMENT ON TABLE "org" IS 'resource Org; managed by ks, do not edit';

ALTER TABLE "org" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "org" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "org";

CREATE POLICY ks_tenant_isolation ON "org" AS PERMISSIVE FOR ALL USING (("org".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("org".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

CREATE TABLE "user_account" (
  "tenant_id" uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  "id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  "email" text NOT NULL CHECK ("email" ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
  "name" text CHECK (length("name") <= 200),
  "last_login" timestamptz,
  "password_hash" text CHECK (length("password_hash") <= 200),
  CONSTRAINT "user_account_pk" PRIMARY KEY (id)
);

COMMENT ON TABLE "user_account" IS 'resource User; managed by ks, do not edit';

CREATE UNIQUE INDEX "user_account_email_uniq" ON "user_account" ("tenant_id", "email");

ALTER TABLE "user_account" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "user_account" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "user_account";

CREATE POLICY ks_tenant_isolation ON "user_account" AS PERMISSIVE FOR ALL USING (("user_account".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("user_account".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

CREATE OR REPLACE FUNCTION ks_guard_immutable() RETURNS trigger AS $$
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
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION ks_guard_append_only() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'this table is append-only' USING ERRCODE = '42501';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION ks_touch_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TABLE IF EXISTS "ks_membership" CASCADE;
CREATE TABLE "ks_membership" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  role text NOT NULL,
  granted_by uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_membership_unique UNIQUE (tenant_id, actor_id, role)
);

ALTER TABLE "ks_membership" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_membership" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "ks_membership";

CREATE POLICY ks_tenant_isolation ON "ks_membership" AS PERMISSIVE FOR ALL USING (("ks_membership".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("ks_membership".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

DROP TABLE IF EXISTS "ks_session" CASCADE;
CREATE TABLE "ks_session" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
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
  CONSTRAINT ks_session_token UNIQUE (token_hash)
);

ALTER TABLE "ks_session" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_session" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_session";

CREATE POLICY ks_public ON "ks_session" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_api_token" CASCADE;
CREATE TABLE "ks_api_token" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  name text NOT NULL,
  token_hash bytea NOT NULL,
  scopes text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at timestamptz,
  CONSTRAINT ks_api_token_hash UNIQUE (token_hash)
);

ALTER TABLE "ks_api_token" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_api_token" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_api_token";

CREATE POLICY ks_public ON "ks_api_token" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_identity_link" CASCADE;
CREATE TABLE "ks_identity_link" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  provider text NOT NULL,
  subject text NOT NULL,
  email text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_identity_link_unique UNIQUE (provider, subject)
);

ALTER TABLE "ks_identity_link" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_identity_link" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_identity_link";

CREATE POLICY ks_public ON "ks_identity_link" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_outbox" CASCADE;
CREATE TABLE "ks_outbox" (
  id uuid NOT NULL PRIMARY KEY,
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
  CONSTRAINT ks_outbox_state CHECK (state IN ('pending','running','done','dead_letter'))
);

ALTER TABLE "ks_outbox" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_outbox" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_outbox";

CREATE POLICY ks_public ON "ks_outbox" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_jobs" CASCADE;
CREATE TABLE "ks_jobs" (
  id uuid NOT NULL PRIMARY KEY,
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
  CONSTRAINT ks_jobs_state CHECK (state IN ('queued','running','done','dead_letter','canceled'))
);

ALTER TABLE "ks_jobs" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_jobs" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_jobs";

CREATE POLICY ks_public ON "ks_jobs" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_scheduled_runs" CASCADE;
CREATE TABLE "ks_scheduled_runs" (
  id uuid NOT NULL PRIMARY KEY,
  schedule text NOT NULL,
  slot text NOT NULL,
  tenant_id uuid,
  job_id uuid,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_scheduled_run_unique UNIQUE (schedule, slot, tenant_id)
);

ALTER TABLE "ks_scheduled_runs" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_scheduled_runs" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_scheduled_runs";

CREATE POLICY ks_public ON "ks_scheduled_runs" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_invocations" CASCADE;
CREATE TABLE "ks_invocations" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  actor_id uuid,
  actor_kind text NOT NULL DEFAULT 'user',
  actor_roles text[] NOT NULL DEFAULT '{}',
  action text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE "ks_invocations" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_invocations" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_invocations";

CREATE POLICY ks_public ON "ks_invocations" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_audit" CASCADE;
CREATE TABLE "ks_audit" (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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
  created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE "ks_audit" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_audit" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "ks_audit";

CREATE POLICY ks_tenant_isolation ON "ks_audit" AS PERMISSIVE FOR ALL USING (("ks_audit".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("ks_audit".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

DROP TABLE IF EXISTS "ks_migrations" CASCADE;
CREATE TABLE "ks_migrations" (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  version text NOT NULL UNIQUE,
  name text NOT NULL,
  checksum text NOT NULL,
  model_hash text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now(),
  destructive_allowed boolean NOT NULL DEFAULT false
);

ALTER TABLE "ks_migrations" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_migrations" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_migrations";

CREATE POLICY ks_public ON "ks_migrations" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_backfills" CASCADE;
CREATE TABLE "ks_backfills" (
  id uuid NOT NULL PRIMARY KEY,
  version text NOT NULL,
  column_name text NOT NULL,
  cursor jsonb,
  rows_done bigint NOT NULL DEFAULT 0,
  total_estimated bigint,
  state text NOT NULL DEFAULT 'pending',
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_backfill_unique UNIQUE (version, column_name)
);

ALTER TABLE "ks_backfills" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_backfills" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_backfills";

CREATE POLICY ks_public ON "ks_backfills" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_idempotency" CASCADE;
CREATE TABLE "ks_idempotency" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid,
  action text NOT NULL,
  key text NOT NULL,
  request_hash bytea NOT NULL,
  response text,
  status_code integer,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  CONSTRAINT ks_idempotency_unique UNIQUE (action, tenant_id, key)
);

ALTER TABLE "ks_idempotency" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_idempotency" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_idempotency";

CREATE POLICY ks_public ON "ks_idempotency" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_rate_bucket" CASCADE;
CREATE TABLE "ks_rate_bucket" (
  id uuid NOT NULL PRIMARY KEY,
  scope text NOT NULL,
  bucket_key text NOT NULL,
  action text NOT NULL,
  tokens double precision NOT NULL,
  max_tokens double precision NOT NULL,
  refill_seconds double precision NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_rate_bucket_unique UNIQUE (action, scope, bucket_key)
);

ALTER TABLE "ks_rate_bucket" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_rate_bucket" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_rate_bucket";

CREATE POLICY ks_public ON "ks_rate_bucket" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_cache_entry" CASCADE;
CREATE TABLE "ks_cache_entry" (
  id uuid NOT NULL PRIMARY KEY,
  cache_key text NOT NULL UNIQUE,
  tenant_id uuid,
  value jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  generation bigint NOT NULL DEFAULT 0
);

ALTER TABLE "ks_cache_entry" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_cache_entry" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_cache_entry";

CREATE POLICY ks_public ON "ks_cache_entry" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

DROP TABLE IF EXISTS "ks_cache_generation" CASCADE;
CREATE TABLE "ks_cache_generation" (
  id uuid NOT NULL PRIMARY KEY,
  tenant_id uuid NOT NULL,
  resource text NOT NULL,
  generation bigint NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ks_cache_generation_unique UNIQUE (tenant_id, resource)
);

ALTER TABLE "ks_cache_generation" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "ks_cache_generation" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_public ON "ks_cache_generation";

CREATE POLICY ks_public ON "ks_cache_generation" AS PERMISSIVE FOR ALL USING (true) WITH CHECK (true);

CREATE INDEX IF NOT EXISTS ks_outbox_claim ON ks_outbox (state, run_after) WHERE state = 'pending';

CREATE INDEX IF NOT EXISTS ks_jobs_claim ON ks_jobs (priority, run_after) WHERE state = 'queued';

CREATE INDEX IF NOT EXISTS ks_session_actor ON ks_session (actor_id);

CREATE INDEX IF NOT EXISTS ks_audit_tenant_time ON ks_audit (tenant_id, created_at);

CREATE TRIGGER ks_audit_guard BEFORE UPDATE OR DELETE ON ks_audit FOR EACH ROW EXECUTE FUNCTION ks_guard_append_only();

REVOKE UPDATE, DELETE, TRUNCATE ON ks_audit FROM PUBLIC;

CREATE TABLE "customer" (
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "email" text NOT NULL CHECK ("email" ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
  "external_id" text CHECK (length("external_id") <= 64),
  "id" uuid NOT NULL,
  "name" text NOT NULL CHECK (length("name") <= 200),
  "owner_id" uuid NOT NULL REFERENCES "user_account"(id) ON DELETE RESTRICT,
  "tenant_id" uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "customer_pk" PRIMARY KEY (id)
);

COMMENT ON TABLE "customer" IS 'resource Customer; managed by ks, do not edit';

CREATE INDEX "customer_owner" ON "customer" ("tenant_id", "owner_id");

CREATE UNIQUE INDEX "customer_email_uniq" ON "customer" ("tenant_id", "email");

ALTER TABLE "customer" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "customer" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "customer";

CREATE POLICY ks_tenant_isolation ON "customer" AS PERMISSIVE FOR ALL USING (("customer".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("customer".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

CREATE TABLE "invoice" (
  "amount_minor" bigint NOT NULL,
  "amount_currency" text NOT NULL DEFAULT 'USD' CHECK ("amount_currency" ~ '^[A-Z]{3}$'),
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "customer_id" uuid NOT NULL REFERENCES "customer"(id) ON DELETE RESTRICT,
  "id" uuid NOT NULL,
  "notes" text CHECK (length("notes") <= 2000),
  "paid_at" timestamptz,
  "sent_at" timestamptz,
  "status" text NOT NULL DEFAULT 'draft' CHECK ("status" IN ('draft','paid','sent','void')),
  "tax_id" bytea,
  "tenant_id" uuid NOT NULL REFERENCES "org"(id) ON DELETE CASCADE,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "invoice_pk" PRIMARY KEY (id),
  CONSTRAINT "invoice_amount_positive" CHECK (invoice.amount_minor > 0),
  CONSTRAINT "invoice_paid_has_date" CHECK ((NOT (invoice.status = 'paid')) OR (invoice.paid_at IS NOT NULL)),
  CONSTRAINT "invoice_sent_has_date" CHECK ((NOT (invoice.status = 'sent')) OR (invoice.sent_at IS NOT NULL)),
  CONSTRAINT "invoice_void_not_paid" CHECK ((NOT (invoice.status = 'void')) OR (NOT (invoice.paid_at IS NOT NULL)))
);

COMMENT ON TABLE "invoice" IS 'resource Invoice; managed by ks, do not edit';

CREATE INDEX "invoice_customer_status" ON "invoice" ("tenant_id", "customer_id", "status");

ALTER TABLE "invoice" ENABLE ROW LEVEL SECURITY;

ALTER TABLE "invoice" FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS ks_tenant_isolation ON "invoice";

CREATE POLICY ks_tenant_isolation ON "invoice" AS PERMISSIVE FOR ALL USING (("invoice".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool)) WITH CHECK (("invoice".tenant_id = coalesce(nullif(current_setting('ks.tenant_id', true), ''), '00000000-0000-0000-0000-000000000000')::uuid OR coalesce(nullif(current_setting('ks.cross_tenant', true), ''), 'off')::bool));

