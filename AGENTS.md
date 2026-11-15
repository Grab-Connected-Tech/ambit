# Ambit — agent instructions

This repository is the Ambit framework, not an app. Do not build an app inside it.

## Building an app with Ambit

Needs Go 1.25+ and Postgres 15+ (`am test` starts a disposable one if none is configured).

```sh
go install ./cmd/am                                  # from this checkout
am init myapp --framework /path/to/this/checkout     # outside this repository
cd myapp
am check && am test
```

From then on, work inside the app and follow the app's own `AGENTS.md`. `am gen` writes
that file and keeps it in step with the spec. Read [example/myapp](example/myapp) for a
finished app, and run `am docs --llm` for the full reference in one file. Do not read the
technical specification unless a doc points you to a section of it.

## Changing the framework itself

- `go test ./...` from the root. After changing codegen or the checker, run
  `go run ../../cmd/am gen` in `example/myapp` and commit the regenerated files.
- A new checker rule needs a `checker.Catalog` entry with a failing and a passing example.
