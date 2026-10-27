# <img src="assets/logo/svg/ambit-mark.svg" alt="" width="28" height="28"> Operations

## Configuration

The server reads `am.yaml` from its working directory, or the file `AM_CONFIG` names.
Environment variables override the file. Values may reference the environment as `${VAR}`
or `${VAR:-default}`. An unknown key, or a provider missing a required setting, stops the
process at startup with a clear message.

```yaml
app: myapp
database:
  url: ${DATABASE_URL}
  replicaUrl: ${DATABASE_REPLICA_URL}   # optional: ReplicaOK() queries read here
  maxConns: 32
secrets:
  smtpPassword: ${SMTP_PASSWORD}
  webhookSecret: ${WEBHOOK_SECRET}
  encryptionKey: ${AM_ENCRYPTION_KEY}
  adminToken: ${AM_ADMIN_TOKEN}
effects:
  email:   {provider: smtp, host: smtp.example.com, port: "587", username: app, password: secret:smtpPassword, from: billing@example.com, templates: templates}
  webhook: {provider: signed, endpoint: https://hooks.example.com/in, secret: secret:webhookSecret}
  storage: {provider: s3, endpoint: https://s3.eu-west-1.amazonaws.com, region: eu-west-1, accessKey: ${S3_KEY}, secretKey: ${S3_SECRET}}
limits:
  requestBodyBytes: 1048576
  actionTimeout: 10s
  statementTimeout: 5s
  queryBudget: 20
  maxPageSize: 100
  maxInFlight: 256
cache:
  shared: true          # also keep cached query results in Postgres, for many instances
api:
  compatibility: none   # internal-only API: breaking changes need no deprecation release
telemetry:
  otlpEndpoint: ${OTEL_EXPORTER_OTLP_ENDPOINT}
  adminPort: 9090
allowedImports:
  - github.com/shopspring/decimal       # third-party packages action code may import
```

Settings reference secrets as `secret:<name>`. Action code never sees secrets.

### Environment variables

| Variable | Meaning |
| --- | --- |
| `AM_DATABASE_URL` | The database (also what `am migrate` and `am dev` use) |
| `AM_REPLICA_URL` | Read replica for `ReplicaOK()` queries |
| `AM_ADDR` | Listen address (default `:8080`) |
| `AM_ROLE` | `web`, `worker` or `all` (default): which loops this process runs |
| `AM_CONFIG` | Path to the config file, when it is not `./am.yaml` |
| `AM_ENCRYPTION_KEY`, `AM_ENCRYPTION_KEYS_OLD` | Keys for `Encrypted()` fields (below) |
| `AM_ADMIN_PORT`, `AM_ADMIN_TOKEN` | The admin listener, and the token pprof requires |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Where traces go |
| `AM_ACTION_TIMEOUT`, `AM_STATEMENT_TIMEOUT`, `AM_QUERY_BUDGET`, `AM_MAX_CONNS` | Override limits |
| `AM_DEV=1` | Turns on the `/_am/*` development API. Never set it in production |
| `AM_TEST_DB_URL` | The Postgres tests clone their databases from |

## Effect providers

| Effect | Providers | Notes |
| --- | --- | --- |
| email | `smtp`, `log` | STARTTLS; templates from `templates/<name>.txt`; the idempotency key becomes the Message-ID |
| webhook | `signed`, `log` | POSTs JSON with `Ambit-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">` |
| storage | `s3`, `log` | Any S3-compatible store, path-style addressing, SigV4 |
| http | built in | Only declared hosts; private, loopback and link-local addresses are refused |
| sms | `log` | Register a vendor provider in code |

An unconfigured effect type uses `log`, which records what would have been sent and succeeds,
so a new app runs without credentials. Webhook receivers verify the signature by computing
HMAC-SHA256 with the shared secret over the timestamp, a dot and the raw body, and comparing.

Deferred effects are delivered at least once from the outbox, with exponential backoff and up
to 8 attempts (about a day), then dead-lettered:

```sh
am outbox list --state dead_letter
am outbox inspect <id>
am outbox replay <id>        # back to pending, attempts reset, same idempotency key
am jobs list|inspect|replay  # the same for jobs
```

## Encryption at rest

Fields declared `s.Encrypted(...)` are sealed with envelope encryption. Each value gets its
own AES-256-GCM key, wrapped by the key-encryption key from config. A sealed value is bound to
its table, column and row, so it cannot be copied into another row or field. Action code only
ever sees plaintext. An app with encrypted fields refuses to start without a key.

```sh
am keys new                         # prints "<id>:<base64>" for AM_ENCRYPTION_KEY
```

To rotate:

1. Generate a new key and set it as `AM_ENCRYPTION_KEY`.
2. Move the old key into `AM_ENCRYPTION_KEYS_OLD` (comma-separated if there are several).
3. Deploy, then run `am keys rotate` with `AM_DATABASE_URL` set. It re-seals every value under
   the new key in batches.
4. Remove the old key.

`am dev` keeps a development key in `.am/dev.key`. `am` keeps `.am/` out of git.

## Authentication

A request is authenticated by a session cookie or by `Authorization: Bearer <api token>`.
Anything else is the anonymous actor. Configure the methods in `app/app.go` with
`s.Identity(...)` ([spec](spec.md#app-level-spec-appappgo)).

- **Sessions** are server-side records. The cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, and
  rotates on login and on privilege change.
- **API tokens** are shown once, stored as SHA-256 hashes, and may be scoped to some actions.
- **Roles** are rows in `am_membership`, per tenant. Revoking one takes effect on the next request.

> **Current limitation.** The generated server has no login, logout, signup or OIDC callback
> endpoints, and actions cannot set cookies. Sessions and tokens are issued from Go through the
> runtime: `app.Login(...)`, `app.NewAPIToken(...)`, `app.Link(...)` for OIDC, and
> `app.GrantRole(...)`. So today an app needs its own small program (an admin script, or a
> custom main that builds the app with `gen.New`) to issue them.

## Deployment

```sh
am build                             # bin/server: one static binary, migrations embedded
docker build -f Dockerfile .         # run from the Go module root; distroless, non-root
```

`am gen` writes the `Dockerfile`. A release is:

1. Run `myapp migrate apply` with the new image (see [Migrations](migrations.md#in-production)).
2. Start the new instances. They refuse to start against an older schema.

Run one process with `AM_ROLE=all`, or scale `web` and `worker` processes separately. Instances
are stateless. Schedules elect a leader through a Postgres advisory lock. On `SIGTERM` a
process stops accepting, drains in-flight requests and jobs, and exits. The Go memory limit is
set to 90% of the container's limit unless `GOMEMLIMIT` is set.

## Health, metrics and tracing

| Endpoint | Where | Meaning |
| --- | --- | --- |
| `/_am/health/live` | main port | The process is up |
| `/_am/health/ready` | main port | The database answers |
| `/metrics` | admin port | Prometheus text: request counts, errors by code, latency, queries per action, pool use, outbox lag and dead letters, job queue depth and age, cache hits |
| `/debug/pprof/` | admin port | Profiles. Requires `Authorization: Bearer <adminToken>`; off without a token |

With an OTLP endpoint configured, each action is exported as a trace: a server span with a
child span per query and per effect, carrying the action, actor kind, tenant and error code.
Statement arguments and payloads are never exported. A caller's W3C `traceparent` header
continues its trace. Logs are JSON lines, each carrying the trace id.

---

<sub>Ambit is built and maintained by [Grab Connected Tech](https://www.grabians.com/). Need a team to build or scale a product on Ambit? Write to [hello@grabians.com](mailto:hello@grabians.com).</sub>
