# Migrations

Migrations are planned from the difference between the app model at the last migration and
the model now. Each step is classified by risk, and a risky step needs an explicit flag to
apply.

```sh
ks migrate plan --name add_due_date   # writes migrations/0002_add_due_date.*
ks migrate verify                     # every migration on a fresh database must match the model
ks migrate apply                      # needs KS_DATABASE_URL
ks migrate status
```

`ks migrate plan` writes three files per migration:

| File | What it is |
| --- | --- |
| `NNNN_name.plan.json` | The classified steps. This is what `apply` executes |
| `NNNN_name.sql` | The same statements, for review |
| `NNNN_name.model.json` | The model after this migration: the base for the next plan |

Applied migrations are immutable. `apply` records each plan's checksum, and an edited applied
plan fails `status` and `apply` (KS-MIG-002). To change something, plan a new migration.

## Change classes

| Change | Class | What the plan does |
| --- | --- | --- |
| Add a resource, an optional field, a field with a default | safe | `CREATE TABLE`, `ADD COLUMN` |
| Add an index | safe | `CREATE INDEX CONCURRENTLY`, outside the transaction |
| Add an invariant | needs validation | `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE` |
| Add a required field without a default | needs backfill | Add nullable, backfill, then `SET NOT NULL` |
| Rename a field | needs plan | Use `.RenameFrom("old")`; otherwise it is a drop plus an add |
| Change a field's type | needs plan | Expand and contract through a new column |
| Remove a field or a resource | destructive | Deprecate it first, then remove it with `--allow-destructive` |

Applying a step that needs a flag fails without it (KS-MIG-001):

```sh
ks migrate apply --plan                # steps that need a reviewed plan
ks migrate apply --allow-destructive   # destructive steps
```

## Backfills

When a required field is added to a table that has rows, the plan adds the column as
nullable, fills it, then sets `NOT NULL` behind a validated check. The value each existing row
gets is the field's default if it has one. Otherwise, fill in `backfill.set` in the plan file
before applying, as a SQL expression:

```json
"backfill": {"table": "customer", "column": "region", "set": "'eu'", "batch": 1000}
```

`apply` refuses a backfill with an empty `set`. Backfills run in batches of 1,000 rows keyed by
primary key, one transaction per batch, and record their progress in `ks_backfills`. An
interrupted run resumes where it stopped.

Migrations run with a 2 s lock timeout (retried with backoff when a lock is busy) and a
15-minute statement timeout.

## In production

The app binary carries its migrations, so the release step runs the same binary you are about
to start:

```sh
myapp migrate status    # exit code 3 while a migration is pending
myapp migrate apply     # add --plan / --allow-destructive as the plan requires
myapp                   # then start the new instances
```

Instances refuse to start against a schema older than their model.

## Framework upgrades

A new Keystone version can generate different policies or indexes for the same model.
`ks upgrade` updates the framework, regenerates, and plans a `framework_upgrade` migration
when that happens. The step re-applies row-level security and brings the indexes in line,
rebuilding them concurrently.
