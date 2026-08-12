# pg_backup_s3 — Go Rewrite Design

**Date:** 2026-08-12
**Status:** Approved, ready for implementation planning

## Goal

Rewrite the existing TypeScript/Node backup daemon in Go. Five requirements drove the design:

1. Refactor — replace the ad-hoc structure with clear, testable units
2. Cleanup — remove dead abstractions, redundant lockfiles, unused dependencies
3. Best performance — bounded memory, no local disk, no wasted round trips
4. Support multiple PostgreSQL versions
5. Drop the requirement that the hourly tier be configured

Deployment target is Docker on Easypanel.

## Problems in the current implementation

These are defects, not style preferences. Each is addressed below.

| # | Problem | Location |
|---|---------|----------|
| 1 | `listObjectsV2` is not paginated — silently caps at 1000 objects, so beyond that old backups are never deleted | `src/s3.ts:44` |
| 2 | `deleteObjects` accepts at most 1000 keys per call — a large cleanup fails | `src/s3.ts:55` |
| 3 | Nothing excludes the just-uploaded object from the delete set | `src/s3.ts:49` |
| 4 | Prune runs even when the dump failed, so a bad backup can rotate away good ones | `src/s3.ts:78` |
| 5 | A thrown `$` inside the cron callback is an unhandled rejection; on Node 24 this terminates the process, silently ending all future backups | `src/index.ts:90` |
| 6 | Tier selection uses `getHours() === 22`, `getDay() === 0`, `getDate() === 1` in container-local time — untestable and timezone-fragile | `src/index.ts:40-71` |
| 7 | `putObject` with a stream of unknown length buffers the whole body in memory | `src/s3.ts:36` |
| 8 | Hourly is mandatory; the deployed `.env` has already moved past this and comments it out | `src/env.ts:16` |
| 9 | Three lockfiles committed (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`) | repo root |
| 10 | `pg_dump` is pinned to one version via `nixpacks.toml`; it refuses to dump any server newer than itself | `nixpacks.toml` |

## Decisions

| Decision | Choice |
|---|---|
| Language | Go 1.26 |
| PostgreSQL version support | Detect server version at runtime, select the matching `pg_dump` binary |
| Dump/upload pipeline | Stream `pg_dump` stdout directly to S3; never touch local disk |
| Scheduling | One independent cron per tier; every tier optional |
| Dump format | Custom (`-Fc`) |
| Bundled `pg_dump` majors | 14–18, overridable via Docker build arg |
| On failure | Log and isolate; skip prune; scheduler survives |
| TypeScript code | Deleted; recoverable from git history |
| Packaging | Multi-stage Dockerfile (replaces nixpacks) |

## Architecture

Single static binary, flat `package main` at repo root. Roughly 450 lines; a `cmd/` + `internal/` split would be ceremony at this size.

| File | Responsibility | Depends on |
|---|---|---|
| `main.go` | Load config, register one cron job per active tier, block until signal | `config.go`, `pg.go`, `s3.go` |
| `config.go` | Parse and validate environment; determine active tiers | stdlib only |
| `pg.go` | Discover installed `pg_dump` binaries, detect server version, select binary, stream dump | stdlib only |
| `s3.go` | Build client, streaming upload, paginated prune | aws-sdk-go-v2 |

### Dependencies

Three, down from seven.

- `github.com/aws/aws-sdk-go-v2/{config,service/s3,feature/s3/manager}`
- `github.com/go-co-op/gocron/v2`
- `github.com/joho/godotenv` — local `.env` convenience only; Easypanel injects real environment variables

Dropped: `zx` → `os/exec`; `date-fns` → `time`; `p-map` → scheduler concurrency limit; `ts-dotenv` → `os.Getenv` + explicit validation.

## Configuration

Variable names are unchanged from the current `.env`.

| Variable | Required | Notes |
|---|---|---|
| `S3_REGION`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `S3_BUCKET`, `S3_ENDPOINT` | yes | |
| `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | yes | |
| `POSTGRES_DATABASE` | yes | comma-separated list; each entry trimmed |
| `SCHEDULE_HOURLY`, `SCHEDULE_DAILY`, `SCHEDULE_WEEKLY`, `SCHEDULE_MONTHLY` | no | 5-field cron expression |
| `BACKUP_KEEP_DAYS_HOURLY`, `..._DAILY`, `..._WEEKLY`, `..._MONTHLY` | no | integer days, must be > 0 |
| `TZ` | no | defaults to `UTC`; controls cron interpretation |
| `S3_FORCE_PATH_STYLE` | no | defaults to `false`, matching current behaviour |

**Tier activation rule:** a tier is active if and only if both its `SCHEDULE_*` and its `BACKUP_KEEP_DAYS_*` are set and valid.

- Only one set → startup error naming the missing variable. Silently ignoring a half-configured tier is how backups go quietly missing.
- Zero active tiers → startup error.
- `BACKUP_KEEP_DAYS_*` ≤ 0 → startup error. Zero would mean "delete everything including what was just written".

This satisfies requirement 5 and removes the hardcoded hour/day/date checks. Weekly is expressed as `0 22 * * 0` rather than `getDay() === 0 && getHours() === 22`.

## PostgreSQL version support

### Constraint

Per the PostgreSQL documentation: *"pg_dump cannot dump from PostgreSQL servers newer than its own major version; it will refuse to even try, rather than risk making an invalid dump."* Dumping **older** servers is supported back to 9.2.

The rule is one-directional, so the client must be greater than or equal to the server.

### Discovery

At startup, scan `/usr/lib/postgresql/*/bin/pg_dump` — the Debian/PGDG layout, where multiple `postgresql-client-N` packages coexist without conflict. Build `map[int]string` from major version to binary path. Empty map → startup error.

The scan directory is overridable via `PG_BIN_DIR` (default `/usr/lib/postgresql`) so tests can point at a fixture tree.

### Detection

Per backup run, per database:

```
psql -tAqc "SHOW server_version_num"
```

Returns e.g. `160004`; major is `value / 10000` under the PostgreSQL 10+ numbering scheme. The binary used is `<PG_BIN_DIR>/<highest installed major>/bin/psql`, chosen unconditionally: any libpq version can execute this query against any server, so no bootstrapping problem exists. Connection parameters are passed via `cmd.Env`, as for `pg_dump`.

Detection runs per backup rather than being cached, so a server upgrade is picked up without a redeploy. The cost is one sub-millisecond query per run.

### Selection

Given server major `S` and installed majors `I`:

1. If `S ∈ I` → use it. Matching majors give the highest-fidelity dump.
2. Else use `min{ i ∈ I : i > S }` — the lowest client newer than the server.
3. Else error: `server is PostgreSQL <S>, no pg_dump >= <S> installed (have: <I>)`.

A client older than the server is never selected.

### Credentials

Connection parameters are passed through `cmd.Env` as `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` rather than as command-line arguments, keeping the password out of `ps` output.

## Dump and upload

`pg_dump -Fc` writes the custom format to stdout when no `-f` is given. Custom format is compressed by default, so no gzip stage is needed. The compression level flag is deliberately omitted: `-Z` takes an integer level on PostgreSQL 15 and earlier but a `method:level` string on 16+, and the default (level 6) is the same across all bundled versions.

```
pg_dump -Fc  →  stdout pipe  →  manager.Uploader  →  S3
```

- `PartSize` 16 MB, `Concurrency` 4 → memory ceiling around 64 MB regardless of database size or how many tiers are due
- `stderr` captured to a bounded buffer (last 8 KB) for error reporting
- `LeavePartsOnError` left at its default of `false`, so a failed multipart upload does not leave billable orphaned parts

### Ordering, and why it matters

`Upload` must complete before `cmd.Wait()` is called — the pipe has to drain or `pg_dump` blocks on a full buffer. This means upload success is known *before* dump success, so:

1. `Upload` returns
2. `cmd.Wait()` — if non-zero exit, **delete the object just written**, return an error carrying stderr, and skip prune
3. Only on a clean exit does prune run

Without step 2, a `pg_dump` that dies partway produces a truncated but perfectly valid S3 object, which then counts as a successful backup and triggers rotation of good ones.

### Object key

```
db_backup/<database>/<tier>/<database>-<RFC3339 timestamp>.dump
```

The timestamp is UTC in RFC 3339 form (Go layout `2006-01-02T15:04:05Z`), matching what `toISOString()` produces today minus milliseconds. Colons are retained rather than sanitised so new keys sort alongside existing ones; they are valid in S3 object keys.

The prefix layout is unchanged, so existing retention continues to work. The extension changes from `.gz` to `.dump` because `-Fc` output is not gzip. Existing `.gz` objects are left alone and age out through normal retention.

Each tier is an independent job and stamps its own timestamp at dump time, so two tiers due at the same instant produce two separate dumps with near-but-not-identical timestamps. This is the accepted cost of the independent-schedule model.

### S3-compatible endpoint detail

`RequestChecksumCalculation` is set to `WhenRequired`. The SDK default (`WhenSupported`) emits `aws-chunked` transfer encoding with trailing checksums, which several S3-compatible providers reject. `UsePathStyle` follows `S3_FORCE_PATH_STYLE`, defaulting to `false` to match `forcePathStyle: false` in the current code.

## Retention

Runs only after a verified-successful dump.

1. Page through all keys under `db_backup/<db>/<tier>/` using `NewListObjectsV2Paginator` (fixes problem 1)
2. Cutoff is `time.Now().Add(-keepDays * 24h)`; select objects whose `LastModified` is strictly before it
3. Exclude the key just uploaded by exact match (fixes problem 3)
4. Delete in batches of at most 1000 (fixes problem 2)
5. Log the number deleted

Step 3 is belt-and-braces given step 2, but a clock skew between the container and the S3 provider would otherwise be able to delete the fresh backup.

## Concurrency and failure isolation

The scheduler is created with `gocron.WithLimitConcurrentJobs(1, gocron.LimitModeWait)`: one backup job at a time process-wide, queued rather than dropped. This is the declarative replacement for the `p-map` concurrency limit and the fix for commit `74777a5` (out-of-memory when several backups ran together). Streaming already bounds memory, so this constraint now exists to protect the database from concurrent dumps — and it matters because independent tiers can legitimately fire at the same instant, for example daily and weekly both at 22:00 on a Sunday.

Within a job, databases are processed sequentially.

Errors are contained at the (tier, database) level: log with captured stderr, delete the partial object, skip prune for that pair, continue to the next database. The scheduler is never torn down. This fixes problem 5.

`SIGINT`/`SIGTERM` cancel the context, allowing an in-flight dump to abort cleanly, then shut the scheduler down.

## Docker and Easypanel

Multi-stage build:

- **Stage 1** — `golang:1.26-bookworm`, `CGO_ENABLED=0 go build`
- **Stage 2** — `debian:bookworm-slim`, PGDG apt repository, `postgresql-client-$v` for each `$v` in `ARG PG_MAJORS="14 15 16 17 18"`

PostgreSQL 13 reached end of life in November 2025 and is excluded; the build arg allows adding 19 without editing the Dockerfile body.

Runs as a non-root user. No volumes and no writable filesystem are required, since nothing touches local disk. Expected image size is roughly 120 MB.

Easypanel setup: GitHub source, build method Dockerfile, environment variables pasted from `.env`. `nixpacks.toml` is deleted.

A `docker-compose.yml` is included for local runs and as a reference for the required environment.

## Testing

Go standard `testing`, table-driven, no framework. S3 and command execution sit behind small interfaces so the decision logic is pure and runs without Docker or a live database.

| Test | Covers |
|---|---|
| `TestPickDumpBinary` | exact match, next-newest fallback, no-suitable-client error, empty install set |
| `TestParseServerVersion` | `160004` → 16, malformed output, trailing whitespace |
| `TestSelectExpiredKeys` | cutoff boundary, pagination beyond 1000, batch chunking at 1000, current key excluded |
| `TestConfigTiers` | tier active only when both variables set, half-configured error, zero-tier error, non-positive keep-days error |

One optional integration check via `docker compose`, against a real PostgreSQL and MinIO, asserting that an object lands and that `pg_restore` reads it back.

## Out of scope

- **Alerting / heartbeat.** Failures are logged only. If silent failure becomes a concern, an optional `HEALTHCHECK_URL` pinged on success and on failure is roughly eight lines of `net/http`.
- **Restore command.** Restore remains a manual `pg_restore` invocation.
- **Server-side copy between tiers.** Each tier takes its own dump. Promoting one dump across tiers via `CopyObject` would save database load, but tiers fire on independent schedules by design, so there is not always a recent dump to promote.
- **Parallel dump (`-j`).** Requires directory format, which cannot stream to stdout.

## Migration

1. Merge the Go implementation; `.env` needs no changes — it already defines the per-tier schedules
2. Switch the Easypanel service build method from Nixpacks to Dockerfile
3. Confirm the first run of each active tier writes a `.dump` object
4. Legacy `.gz` objects expire on their own; no manual cleanup needed
