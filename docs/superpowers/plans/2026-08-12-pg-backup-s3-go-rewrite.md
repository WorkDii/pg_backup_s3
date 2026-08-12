# pg_backup_s3 Go Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the TypeScript PostgreSQL-to-S3 backup daemon with a single Go binary that selects a `pg_dump` matching each server's version, streams dumps straight to S3 without touching disk, and schedules every retention tier independently.

**Architecture:** One `package main` at repo root, four source files with distinct responsibilities. `config.go` parses and validates environment into a `Config` with a list of active `Tier`s. `pg.go` discovers installed PostgreSQL client binaries, detects each server's major version, picks a compatible `pg_dump`, and exposes its stdout as a stream. `s3.go` wraps the AWS SDK uploader and paginated retention pruning. `main.go` wires a `gocron` scheduler with one job per tier. Decision logic is kept in pure functions so it is unit-testable without Docker, a database, or network access.

**Tech Stack:** Go 1.26, `aws-sdk-go-v2` (`service/s3`, `feature/s3/manager`, `credentials`), `go-co-op/gocron/v2`, `joho/godotenv`, stdlib `os/exec` and `testing`. Multi-stage Dockerfile on `debian:bookworm-slim` with PGDG client packages.

**Spec:** `docs/superpowers/specs/2026-08-12-pg-backup-s3-go-rewrite-design.md`

## Global Constraints

- Go version: **1.26** (`go.mod` declares `go 1.26`; build image `golang:1.26-bookworm`)
- Total non-stdlib direct dependencies: **3 logical** — `aws-sdk-go-v2`, `gocron/v2`, `godotenv`. Do not add others. Note `aws-sdk-go-v2` is distributed as several Go modules (`service/s3`, `feature/s3/manager`, `credentials`, and the core module for the `aws` package), so `go.mod` will list more than three `require` lines. That is expected and is not a violation of this constraint.
- Module path: `pg_backup_s3` — a bare name is valid because this binary is never imported by another module.
- All source files live at repo root in `package main`. No `cmd/` or `internal/` directories.
- Environment variable names are **unchanged** from the existing `.env`. Do not rename any.
- A tier is active **if and only if** both `SCHEDULE_<TIER>` and `BACKUP_KEEP_DAYS_<TIER>` are set and valid. Half-configured is a startup error, never a silent skip.
- `pg_dump` selection must **never** choose a client older than the server major.
- Prune runs **only** after a dump exits zero.
- Object key format: `db_backup/<database>/<tier>/<database>-<UTC RFC3339>.dump` using Go layout `2006-01-02T15:04:05Z`.
- S3 client must set `RequestChecksumCalculation` to `WhenRequired`.
- Uploader: `PartSize` 16 MiB, `Concurrency` 4, `LeavePartsOnError` left at default `false`.
- Delete batches: maximum 1000 keys per `DeleteObjects` call.
- Never write dump data to local disk.
- Commit after every task.

---

### Task 1: Module scaffolding and configuration

**Files:**
- Create: `go.mod`
- Create: `config.go`
- Create: `.gitignore` (modify existing)
- Test: `config_test.go`

**Interfaces:**
- Consumes: nothing (first task)
- Produces:
  - `type Tier struct { Name string; Cron string; KeepDays int }`
  - `type Config struct` with fields `S3Region, S3AccessKeyID, S3SecretKey, S3Bucket, S3Endpoint string`, `S3ForcePathStyle bool`, `PGHost, PGPort, PGUser, PGPassword string`, `Databases []string`, `Tiers []Tier`, `Location *time.Location`, `PGBinDir string`
  - `func LoadConfig(getenv func(string) string) (*Config, error)`
  - `func (c *Config) PGEnv(database string) []string`

- [ ] **Step 1: Initialise the Go module**

```bash
go mod init pg_backup_s3
go mod edit -go=1.26
```

- [ ] **Step 2: Replace `.gitignore` with Go-appropriate contents**

The existing file is Node-oriented. Replace its entire contents with:

```gitignore
.env
.env*
!.env.example

pg_backup_s3
pg_backup_s3.exe

# Node leftovers, still present on disk until Task 8 removes them. Keeping
# them ignored means an accidental `git add -A` in an intervening task
# cannot commit node_modules.
node_modules
/dist/

*.dump
*.gz

.superpowers/
```

- [ ] **Step 3: Write the failing tests**

Create `config_test.go`:

```go
package main

import "testing"

// envMap returns a getenv function backed by a map, so tests never touch the
// real process environment.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// baseEnv is a minimal valid environment: all required vars, one active tier.
func baseEnv() map[string]string {
	return map[string]string{
		"S3_REGION":               "us-east-1",
		"S3_ACCESS_KEY_ID":        "key",
		"S3_SECRET_ACCESS_KEY":    "secret",
		"S3_BUCKET":               "bucket",
		"S3_ENDPOINT":             "https://example.com",
		"POSTGRES_HOST":           "db.example.com",
		"POSTGRES_PORT":           "5432",
		"POSTGRES_USER":           "postgres",
		"POSTGRES_PASSWORD":       "pw",
		"POSTGRES_DATABASE":       "app",
		"SCHEDULE_DAILY":          "0 22 * * *",
		"BACKUP_KEEP_DAYS_DAILY":  "7",
	}
}

func TestConfigTiers(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(map[string]string)
		wantTiers []Tier
		wantErr   bool
	}{
		{
			name:      "single daily tier",
			mutate:    func(m map[string]string) {},
			wantTiers: []Tier{{Name: "daily", Cron: "0 22 * * *", KeepDays: 7}},
		},
		{
			name: "hourly is not required",
			mutate: func(m map[string]string) {
				m["SCHEDULE_MONTHLY"] = "0 22 1 * *"
				m["BACKUP_KEEP_DAYS_MONTHLY"] = "365"
			},
			wantTiers: []Tier{
				{Name: "daily", Cron: "0 22 * * *", KeepDays: 7},
				{Name: "monthly", Cron: "0 22 1 * *", KeepDays: 365},
			},
		},
		{
			name: "schedule without keep days is an error",
			mutate: func(m map[string]string) {
				m["SCHEDULE_WEEKLY"] = "0 22 * * 0"
			},
			wantErr: true,
		},
		{
			name: "keep days without schedule is an error",
			mutate: func(m map[string]string) {
				m["BACKUP_KEEP_DAYS_WEEKLY"] = "30"
			},
			wantErr: true,
		},
		{
			name: "zero keep days is an error",
			mutate: func(m map[string]string) {
				m["BACKUP_KEEP_DAYS_DAILY"] = "0"
			},
			wantErr: true,
		},
		{
			name: "non-numeric keep days is an error",
			mutate: func(m map[string]string) {
				m["BACKUP_KEEP_DAYS_DAILY"] = "seven"
			},
			wantErr: true,
		},
		{
			name: "no active tiers is an error",
			mutate: func(m map[string]string) {
				delete(m, "SCHEDULE_DAILY")
				delete(m, "BACKUP_KEEP_DAYS_DAILY")
			},
			wantErr: true,
		},
		{
			name: "missing required var is an error",
			mutate: func(m map[string]string) {
				delete(m, "S3_BUCKET")
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := baseEnv()
			tc.mutate(m)
			cfg, err := LoadConfig(envMap(m))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cfg.Tiers) != len(tc.wantTiers) {
				t.Fatalf("got %d tiers, want %d: %+v", len(cfg.Tiers), len(tc.wantTiers), cfg.Tiers)
			}
			for i, want := range tc.wantTiers {
				if cfg.Tiers[i] != want {
					t.Errorf("tier %d = %+v, want %+v", i, cfg.Tiers[i], want)
				}
			}
		})
	}
}

func TestConfigDatabases(t *testing.T) {
	m := baseEnv()
	m["POSTGRES_DATABASE"] = " app , analytics ,, billing "
	cfg, err := LoadConfig(envMap(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"app", "analytics", "billing"}
	if len(cfg.Databases) != len(want) {
		t.Fatalf("got %v, want %v", cfg.Databases, want)
	}
	for i := range want {
		if cfg.Databases[i] != want[i] {
			t.Errorf("database %d = %q, want %q", i, cfg.Databases[i], want[i])
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Location.String() != "UTC" {
		t.Errorf("Location = %q, want UTC", cfg.Location)
	}
	if cfg.PGBinDir != "/usr/lib/postgresql" {
		t.Errorf("PGBinDir = %q, want /usr/lib/postgresql", cfg.PGBinDir)
	}
	if cfg.S3ForcePathStyle {
		t.Errorf("S3ForcePathStyle = true, want false")
	}
}

func TestPGEnvCarriesCredentials(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := cfg.PGEnv("analytics")
	want := map[string]bool{
		"PGHOST=db.example.com": false,
		"PGPORT=5432":           false,
		"PGUSER=postgres":       false,
		"PGPASSWORD=pw":         false,
		"PGDATABASE=analytics":  false,
	}
	for _, kv := range got {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for kv, found := range want {
		if !found {
			t.Errorf("PGEnv missing %q", kv)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./... -run 'TestConfig|TestPGEnv' -v`
Expected: FAIL — compile error, `undefined: LoadConfig`, `undefined: Tier`

- [ ] **Step 5: Implement `config.go`**

```go
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// tierNames is ordered from most to least frequent. Config.Tiers preserves
// this order so startup logs read predictably.
var tierNames = []string{"hourly", "daily", "weekly", "monthly"}

// Tier is one retention schedule: when to back up, and how long to keep.
type Tier struct {
	Name     string
	Cron     string
	KeepDays int
}

// Config is the fully validated runtime configuration. Every field is
// populated by LoadConfig; there are no optional-at-runtime fields.
type Config struct {
	S3Region         string
	S3AccessKeyID    string
	S3SecretKey      string
	S3Bucket         string
	S3Endpoint       string
	S3ForcePathStyle bool

	PGHost     string
	PGPort     string
	PGUser     string
	PGPassword string
	Databases  []string

	Tiers    []Tier
	Location *time.Location
	PGBinDir string
}

// LoadConfig reads configuration through getenv, which is injected so tests
// never depend on the real process environment. All validation problems are
// collected and reported together rather than one per run.
func LoadConfig(getenv func(string) string) (*Config, error) {
	var problems []string

	required := func(key string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			problems = append(problems, key+" is required")
		}
		return v
	}

	cfg := &Config{
		S3Region:      required("S3_REGION"),
		S3AccessKeyID: required("S3_ACCESS_KEY_ID"),
		S3SecretKey:   required("S3_SECRET_ACCESS_KEY"),
		S3Bucket:      required("S3_BUCKET"),
		S3Endpoint:    required("S3_ENDPOINT"),
		PGHost:        required("POSTGRES_HOST"),
		PGPort:        required("POSTGRES_PORT"),
		PGUser:        required("POSTGRES_USER"),
		PGPassword:    required("POSTGRES_PASSWORD"),
	}

	for _, name := range strings.Split(getenv("POSTGRES_DATABASE"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.Databases = append(cfg.Databases, name)
		}
	}
	if len(cfg.Databases) == 0 {
		problems = append(problems, "POSTGRES_DATABASE is required (comma-separated list)")
	}

	for _, name := range tierNames {
		upper := strings.ToUpper(name)
		cron := strings.TrimSpace(getenv("SCHEDULE_" + upper))
		keep := strings.TrimSpace(getenv("BACKUP_KEEP_DAYS_" + upper))

		switch {
		case cron == "" && keep == "":
			continue // tier not configured, which is fine
		case cron == "":
			problems = append(problems, fmt.Sprintf(
				"BACKUP_KEEP_DAYS_%s is set but SCHEDULE_%s is missing", upper, upper))
			continue
		case keep == "":
			problems = append(problems, fmt.Sprintf(
				"SCHEDULE_%s is set but BACKUP_KEEP_DAYS_%s is missing", upper, upper))
			continue
		}

		days, err := strconv.Atoi(keep)
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"BACKUP_KEEP_DAYS_%s must be a whole number, got %q", upper, keep))
			continue
		}
		if days <= 0 {
			problems = append(problems, fmt.Sprintf(
				"BACKUP_KEEP_DAYS_%s must be greater than 0, got %d", upper, days))
			continue
		}

		cfg.Tiers = append(cfg.Tiers, Tier{Name: name, Cron: cron, KeepDays: days})
	}
	if len(cfg.Tiers) == 0 {
		problems = append(problems, "no backup tier is configured: set SCHEDULE_<TIER> and "+
			"BACKUP_KEEP_DAYS_<TIER> for at least one of hourly, daily, weekly, monthly")
	}

	tz := strings.TrimSpace(getenv("TZ"))
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		problems = append(problems, fmt.Sprintf("TZ %q is not a known timezone", tz))
		loc = time.UTC
	}
	cfg.Location = loc

	cfg.PGBinDir = strings.TrimSpace(getenv("PG_BIN_DIR"))
	if cfg.PGBinDir == "" {
		cfg.PGBinDir = "/usr/lib/postgresql"
	}

	if v := strings.TrimSpace(getenv("S3_FORCE_PATH_STYLE")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"S3_FORCE_PATH_STYLE must be true or false, got %q", v))
		}
		cfg.S3ForcePathStyle = b
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// PGEnv returns the environment for a psql or pg_dump child process.
// Credentials travel here rather than in argv so the password never appears
// in the output of ps.
func (c *Config) PGEnv(database string) []string {
	return append(os.Environ(),
		"PGHOST="+c.PGHost,
		"PGPORT="+c.PGPort,
		"PGUSER="+c.PGUser,
		"PGPASSWORD="+c.PGPassword,
		"PGDATABASE="+database,
		"PGCONNECT_TIMEOUT=10",
	)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./... -run 'TestConfig|TestPGEnv' -v`
Expected: PASS — all subtests green

- [ ] **Step 7: Commit**

```bash
git add go.mod config.go config_test.go .gitignore
git commit -m "feat: add Go module and validated configuration

Tiers activate only when both SCHEDULE_<TIER> and BACKUP_KEEP_DAYS_<TIER>
are set, making hourly optional. Half-configured tiers fail at startup
rather than being silently skipped."
```

---

### Task 2: PostgreSQL client discovery and version selection

**Files:**
- Create: `pg.go`
- Test: `pg_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (pure logic, independent)
- Produces:
  - `type Installed map[int]string` — major version to client `bin` directory
  - `func Discover(root string) (Installed, error)`
  - `func (in Installed) Majors() []int` — sorted ascending
  - `func (in Installed) Newest() string` — bin directory of the highest major
  - `func (in Installed) Pick(serverMajor int) (string, error)` — bin directory to use
  - `func parseServerVersion(out string) (int, error)`

- [ ] **Step 1: Write the failing tests**

Create `pg_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeInstall builds a directory tree shaped like Debian's PostgreSQL client
// layout: <root>/<major>/bin/{pg_dump,psql}
func fakeInstall(t *testing.T, majors ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, m := range majors {
		bin := filepath.Join(root, m, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, prog := range []string{"pg_dump", "psql"} {
			if err := os.WriteFile(filepath.Join(bin, prog), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestParseServerVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "postgres 16", in: "160004", want: 16},
		{name: "postgres 18", in: "180001", want: 18},
		{name: "trailing newline", in: "140012\n", want: 14},
		{name: "surrounding whitespace", in: "  150006  \n", want: 15},
		{name: "empty output", in: "", wantErr: true},
		{name: "not a number", in: "sixteen", wantErr: true},
		{name: "pre-10 numbering is rejected", in: "90605", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseServerVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	root := fakeInstall(t, "14", "16", "18")
	in, err := Discover(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{14, 16, 18}
	got := in.Majors()
	if len(got) != len(want) {
		t.Fatalf("got majors %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got majors %v, want %v", got, want)
		}
	}
}

func TestDiscoverIgnoresJunkAndEmptyRoots(t *testing.T) {
	root := fakeInstall(t, "16")
	// A non-numeric directory, and a numeric one with no pg_dump inside.
	if err := os.MkdirAll(filepath.Join(root, "notes", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "17", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := Discover(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in) != 1 || in[16] == "" {
		t.Errorf("got %v, want only major 16", in.Majors())
	}

	if _, err := Discover(t.TempDir()); err == nil {
		t.Error("expected an error when no clients are installed")
	}
	if _, err := Discover(filepath.Join(root, "does-not-exist")); err == nil {
		t.Error("expected an error when the root does not exist")
	}
}

func TestPickDumpBinary(t *testing.T) {
	root := fakeInstall(t, "14", "15", "16", "17", "18")
	in, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		serverMajor int
		wantMajor   string
		wantErr     bool
	}{
		{name: "exact match preferred", serverMajor: 16, wantMajor: "16"},
		{name: "exact match at the top", serverMajor: 18, wantMajor: "18"},
		{name: "older server uses lowest newer client", serverMajor: 13, wantMajor: "14"},
		{name: "much older server still uses lowest newer client", serverMajor: 11, wantMajor: "14"},
		{name: "server newer than every client is an error", serverMajor: 19, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := in.Pick(tc.serverMajor)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := filepath.Join(root, tc.wantMajor, "bin")
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestPickNeverChoosesOlderThanServer(t *testing.T) {
	// Only a 14 client installed; a 16 server must be refused outright rather
	// than dumped with an older client, which pg_dump would reject anyway.
	root := fakeInstall(t, "14")
	in, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := in.Pick(16); err == nil {
		t.Errorf("expected an error, got %q", got)
	}
}

func TestNewest(t *testing.T) {
	root := fakeInstall(t, "14", "18", "16")
	in, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "18", "bin")
	if got := in.Newest(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{max: 10}
	if _, err := b.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "6789ABCDEF"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	short := &boundedBuffer{max: 10}
	if _, err := short.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got, want := short.String(), "abc"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestParseServerVersion|TestDiscover|TestPick|TestNewest|TestBoundedBuffer' -v`
Expected: FAIL — compile error, `undefined: Discover`, `undefined: parseServerVersion`, `undefined: boundedBuffer`

- [ ] **Step 3: Implement discovery and selection in `pg.go`**

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Installed maps a PostgreSQL major version to that version's client bin
// directory, e.g. 16 -> /usr/lib/postgresql/16/bin
type Installed map[int]string

// Discover scans the Debian/PGDG client layout <root>/<major>/bin/pg_dump.
// Directories that are not numeric, or that lack pg_dump, are ignored.
func Discover(root string) (Installed, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading PostgreSQL client directory %s: %w", root, err)
	}

	in := Installed{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		major, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		bin := filepath.Join(root, e.Name(), "bin")
		if _, err := os.Stat(filepath.Join(bin, "pg_dump")); err != nil {
			continue
		}
		in[major] = bin
	}

	if len(in) == 0 {
		return nil, fmt.Errorf("no pg_dump found under %s", root)
	}
	return in, nil
}

// Majors returns the installed major versions in ascending order.
func (in Installed) Majors() []int {
	majors := make([]int, 0, len(in))
	for m := range in {
		majors = append(majors, m)
	}
	sort.Ints(majors)
	return majors
}

// Newest returns the bin directory of the highest installed major. Any libpq
// version can run the version-detection query against any server, so this is
// always a safe choice for psql.
func (in Installed) Newest() string {
	majors := in.Majors()
	return in[majors[len(majors)-1]]
}

// Pick returns the client bin directory to use for a server of the given
// major version.
//
// pg_dump refuses to dump a server newer than itself, so the client must be
// greater than or equal to the server. An exact major match is preferred
// because it produces the highest-fidelity dump; otherwise the lowest client
// newer than the server is used.
func (in Installed) Pick(serverMajor int) (string, error) {
	if bin, ok := in[serverMajor]; ok {
		return bin, nil
	}
	best := -1
	for major := range in {
		if major > serverMajor && (best == -1 || major < best) {
			best = major
		}
	}
	if best == -1 {
		return "", fmt.Errorf(
			"server is PostgreSQL %d but no pg_dump >= %d is installed (have %v)",
			serverMajor, serverMajor, in.Majors())
	}
	return in[best], nil
}

// parseServerVersion converts the output of "SHOW server_version_num" into a
// major version. PostgreSQL 10 and later use MMmmmm, so 160004 is major 16.
//
// Releases before 10 used a different scheme (90605 meant 9.6.5) and are
// rejected: every bundled client is 14 or newer, and 9.x has been end-of-life
// since 2021.
func parseServerVersion(out string) (int, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, fmt.Errorf("server_version_num query returned no output")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("unexpected server_version_num %q", s)
	}
	if n < 100000 {
		return 0, fmt.Errorf(
			"server_version_num %d indicates PostgreSQL 9.x or older, which is not supported", n)
	}
	return n / 10000, nil
}

// boundedBuffer keeps only the most recent max bytes written to it. It backs
// child-process stderr so a runaway error stream cannot exhaust memory while
// still preserving the tail, which is the part that explains the failure.
type boundedBuffer struct {
	max int
	buf []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return strings.TrimSpace(string(b.buf)) }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestParseServerVersion|TestDiscover|TestPick|TestNewest|TestBoundedBuffer' -v`
Expected: PASS — all subtests green

- [ ] **Step 5: Commit**

```bash
git add pg.go pg_test.go
git commit -m "feat: discover PostgreSQL clients and select by server version

pg_dump cannot dump a server newer than itself, so Pick prefers an exact
major match and otherwise falls back to the lowest newer client, never an
older one."
```

---

### Task 3: Version detection and dump streaming

**Files:**
- Modify: `pg.go` (append)

**Interfaces:**
- Consumes: `Installed`, `parseServerVersion`, `boundedBuffer` (Task 2); `Config.PGEnv` (Task 1)
- Produces:
  - `func DetectMajor(ctx context.Context, binDir string, env []string) (int, error)`
  - `type Dump struct { Stdout io.ReadCloser; ... }`
  - `func StartDump(ctx context.Context, binDir string, env []string) (*Dump, error)`
  - `func (d *Dump) Wait() error`
  - `func (d *Dump) Terminate()`

There is no unit test in this task. It is pure process orchestration with no
branching logic worth isolating; the pure parts (`parseServerVersion`,
`boundedBuffer`) are already covered by Task 2, and behaviour is verified by
the integration check in Task 8.

- [ ] **Step 1: Append detection and dump streaming to `pg.go`**

Add these imports to the existing import block in `pg.go`: `bytes`, `context`, `io`, `os/exec`.

```go
// DetectMajor asks the server for its own version. The psql binary comes from
// the newest installed client because libpq speaks to any server version for
// a query this simple.
//
// This runs on every backup rather than being cached at startup, so a server
// upgrade is picked up without redeploying. The cost is one trivial query.
func DetectMajor(ctx context.Context, binDir string, env []string) (int, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "psql"), "-tAqc", "SHOW server_version_num")
	cmd.Env = env

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return 0, fmt.Errorf("psql: %w: %s", err, msg)
		}
		return 0, fmt.Errorf("psql: %w", err)
	}
	return parseServerVersion(string(out))
}

// Dump is a running pg_dump whose custom-format output is readable from
// Stdout. The caller must fully drain Stdout before calling Wait, otherwise
// pg_dump blocks on a full pipe.
type Dump struct {
	Stdout io.ReadCloser

	cmd    *exec.Cmd
	stderr *boundedBuffer
}

// StartDump launches pg_dump in custom format writing to stdout.
//
// The compression flag is deliberately omitted: -Z takes a bare integer on
// PostgreSQL 15 and earlier but a method:level string on 16 and later, and
// the default (level 6) is identical across every bundled version.
func StartDump(ctx context.Context, binDir string, env []string) (*Dump, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "pg_dump"), "--format=custom", "--no-password")
	cmd.Env = env

	stderr := &boundedBuffer{max: 8 << 10}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pg_dump stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting pg_dump: %w", err)
	}
	return &Dump{Stdout: stdout, cmd: cmd, stderr: stderr}, nil
}

// Wait reaps pg_dump and reports a non-zero exit with its stderr tail
// attached. Call it only after Stdout has been drained or Terminate called.
func (d *Dump) Wait() error {
	if err := d.cmd.Wait(); err != nil {
		if msg := d.stderr.String(); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Terminate kills pg_dump and closes the read end of its output. It is used
// when the upload fails part-way, where pg_dump would otherwise block forever
// writing into a pipe nobody is reading.
func (d *Dump) Terminate() {
	_ = d.Stdout.Close()
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
}
```

- [ ] **Step 2: Verify the package still builds and all existing tests pass**

Run: `go build ./... && go test ./... -v`
Expected: build succeeds; all Task 1 and Task 2 tests PASS

- [ ] **Step 3: Verify the code is well-formed and vet-clean**

Run: `gofmt -l . && go vet ./...`
Expected: no output from either command

- [ ] **Step 4: Commit**

```bash
git add pg.go
git commit -m "feat: detect server version and stream pg_dump output

Custom format is written to stdout so it can be piped straight to S3.
Terminate unblocks pg_dump when an upload fails mid-stream."
```

---

### Task 4: Retention selection logic

**Files:**
- Create: `s3.go`
- Test: `s3_test.go`

**Interfaces:**
- Consumes: nothing (pure logic)
- Produces:
  - `type Object struct { Key string; Modified time.Time }`
  - `func selectExpired(objs []Object, cutoff time.Time, keep string) []string`
  - `func chunk(keys []string, size int) [][]string`

- [ ] **Step 1: Write the failing tests**

Create `s3_test.go`:

```go
package main

import (
	"fmt"
	"testing"
	"time"
)

func TestSelectExpired(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -7)

	objs := []Object{
		{Key: "old-a", Modified: cutoff.Add(-time.Hour)},
		{Key: "exactly-at-cutoff", Modified: cutoff},
		{Key: "recent", Modified: now.Add(-time.Hour)},
		{Key: "old-b", Modified: cutoff.Add(-48 * time.Hour)},
		{Key: "just-uploaded", Modified: now},
	}

	got := selectExpired(objs, cutoff, "just-uploaded")

	want := map[string]bool{"old-a": true, "old-b": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected key selected for deletion: %q", k)
		}
	}
}

func TestSelectExpiredNeverDeletesTheCurrentKey(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// A clock skew scenario: the object just written is dated well before the
	// cutoff. It must still survive, because deleting it would destroy the
	// backup that was just taken.
	objs := []Object{{Key: "current", Modified: now.AddDate(0, 0, -30)}}

	if got := selectExpired(objs, now, "current"); len(got) != 0 {
		t.Errorf("got %v, want the current key to be kept", got)
	}
}

func TestSelectExpiredEmptyInput(t *testing.T) {
	if got := selectExpired(nil, time.Now(), "x"); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestChunk(t *testing.T) {
	tests := []struct {
		name       string
		total      int
		size       int
		wantChunks []int
	}{
		{name: "empty", total: 0, size: 1000, wantChunks: nil},
		{name: "under one batch", total: 3, size: 1000, wantChunks: []int{3}},
		{name: "exactly one batch", total: 1000, size: 1000, wantChunks: []int{1000}},
		{name: "just over one batch", total: 1001, size: 1000, wantChunks: []int{1000, 1}},
		{name: "several batches", total: 2500, size: 1000, wantChunks: []int{1000, 1000, 500}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keys := make([]string, tc.total)
			for i := range keys {
				keys[i] = fmt.Sprintf("key-%d", i)
			}
			got := chunk(keys, tc.size)
			if len(got) != len(tc.wantChunks) {
				t.Fatalf("got %d batches, want %d", len(got), len(tc.wantChunks))
			}
			seen := 0
			for i, batch := range got {
				if len(batch) != tc.wantChunks[i] {
					t.Errorf("batch %d has %d keys, want %d", i, len(batch), tc.wantChunks[i])
				}
				seen += len(batch)
			}
			if seen != tc.total {
				t.Errorf("batches cover %d keys, want %d", seen, tc.total)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestSelectExpired|TestChunk' -v`
Expected: FAIL — compile error, `undefined: Object`, `undefined: selectExpired`, `undefined: chunk`

- [ ] **Step 3: Implement the pure selection logic in `s3.go`**

```go
package main

import "time"

// Object is the subset of an S3 listing entry that retention cares about.
type Object struct {
	Key      string
	Modified time.Time
}

// selectExpired returns the keys older than cutoff, always excluding keep.
//
// keep is the object written by the run that triggered this prune. Excluding
// it explicitly means a clock skew between this container and the storage
// provider can never delete the backup that was just taken.
func selectExpired(objs []Object, cutoff time.Time, keep string) []string {
	var expired []string
	for _, o := range objs {
		if o.Key == keep {
			continue
		}
		if o.Modified.Before(cutoff) {
			expired = append(expired, o.Key)
		}
	}
	return expired
}

// chunk splits keys into batches of at most size. S3 DeleteObjects accepts a
// maximum of 1000 keys per request.
func chunk(keys []string, size int) [][]string {
	var batches [][]string
	for len(keys) > 0 {
		n := size
		if len(keys) < n {
			n = len(keys)
		}
		batches = append(batches, keys[:n])
		keys = keys[n:]
	}
	return batches
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestSelectExpired|TestChunk' -v`
Expected: PASS — all subtests green

- [ ] **Step 5: Commit**

```bash
git add s3.go s3_test.go
git commit -m "feat: add retention selection and delete batching

Batching at 1000 respects the DeleteObjects limit, and the just-uploaded
key is always excluded from the delete set."
```

---

### Task 5: S3 client, streaming upload, paginated prune

**Files:**
- Modify: `s3.go` (append)

**Interfaces:**
- Consumes: `Config` (Task 1), `Object`, `selectExpired`, `chunk` (Task 4)
- Produces:
  - `type Store struct`
  - `func NewStore(cfg *Config) *Store`
  - `func (s *Store) Upload(ctx context.Context, key string, body io.Reader) error`
  - `func (s *Store) Delete(ctx context.Context, key string) error`
  - `func (s *Store) Prune(ctx context.Context, prefix string, cutoff time.Time, keep string) (int, error)`

- [ ] **Step 1: Add the AWS SDK dependencies**

```bash
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/aws/aws-sdk-go-v2/feature/s3/manager
go get github.com/aws/aws-sdk-go-v2/credentials
```

After writing the code in Step 2, run `go mod tidy` so the core
`github.com/aws/aws-sdk-go-v2` module (which provides the `aws` package used
below) is recorded as a direct requirement.

- [ ] **Step 2: Append the storage layer to `s3.go`**

Add these imports to the existing import block in `s3.go`: `context`, `fmt`, `io`, plus
`github.com/aws/aws-sdk-go-v2/aws`, `github.com/aws/aws-sdk-go-v2/credentials`,
`github.com/aws/aws-sdk-go-v2/feature/s3/manager`,
`github.com/aws/aws-sdk-go-v2/service/s3`,
`github.com/aws/aws-sdk-go-v2/service/s3/types`.

```go
const (
	uploadPartSize    = 16 << 20 // 16 MiB
	uploadConcurrency = 4
	deleteBatchSize   = 1000
)

// Store is the S3 destination for dumps.
type Store struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewStore builds an S3 client from static credentials.
//
// RequestChecksumCalculation is set to WhenRequired because the SDK default
// emits aws-chunked transfer encoding with trailing checksums, which several
// S3-compatible providers reject.
func NewStore(cfg *Config) *Store {
	client := s3.New(s3.Options{
		Region:       cfg.S3Region,
		BaseEndpoint: aws.String(cfg.S3Endpoint),
		UsePathStyle: cfg.S3ForcePathStyle,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.S3AccessKeyID, cfg.S3SecretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = uploadConcurrency
	})

	return &Store{client: client, uploader: uploader, bucket: cfg.S3Bucket}
}

// Upload streams body to key using multipart upload.
//
// Memory stays bounded at roughly PartSize times Concurrency regardless of
// how large the dump turns out to be, because body is never buffered whole.
// LeavePartsOnError is left at its default of false, so a failed upload
// aborts its multipart session instead of leaving billable orphaned parts.
func (s *Store) Upload(ctx context.Context, key string, body io.Reader) error {
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("uploading %s: %w", key, err)
	}
	return nil
}

// Delete removes a single object. It is used to clean up the object written
// by a dump that turned out to have failed.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	return nil
}

// Prune deletes objects under prefix older than cutoff, never touching keep.
// It returns the number of objects deleted.
//
// The listing is paginated: a plain ListObjectsV2 call stops at 1000 objects,
// which silently disables retention once a prefix grows past that.
func (s *Store) Prune(ctx context.Context, prefix string, cutoff time.Time, keep string) (int, error) {
	var objs []Object

	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			objs = append(objs, Object{
				Key:      aws.ToString(o.Key),
				Modified: aws.ToTime(o.LastModified),
			})
		}
	}

	expired := selectExpired(objs, cutoff, keep)
	for _, batch := range chunk(expired, deleteBatchSize) {
		ids := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return 0, fmt.Errorf("deleting %d objects under %s: %w", len(batch), prefix, err)
		}
	}
	return len(expired), nil
}
```

- [ ] **Step 3: Tidy modules, then verify the package builds and all existing tests pass**

Run: `go mod tidy && go build ./... && go test ./... -v`
Expected: build succeeds; all Task 1, 2 and 4 tests PASS

- [ ] **Step 4: Verify formatting and vet**

Run: `gofmt -l . && go vet ./...`
Expected: no output from either command

- [ ] **Step 5: Commit**

```bash
git add s3.go go.mod go.sum
git commit -m "feat: add streaming S3 upload and paginated prune

Fixes three retention defects carried over from the TypeScript version:
unpaginated listing capped at 1000 objects, unbatched deletes exceeding
the 1000-key limit, and no exclusion of the just-uploaded object."
```

---

### Task 6: Orchestration and scheduler

**Files:**
- Create: `main.go`
- Create: `.env.example`

**Interfaces:**
- Consumes: everything produced by Tasks 1–5
- Produces: the `pg_backup_s3` binary entry point

- [ ] **Step 1: Add the remaining dependencies**

```bash
go get github.com/go-co-op/gocron/v2
go get github.com/joho/godotenv
```

- [ ] **Step 2: Implement `main.go`**

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	// tzdata is embedded because debian-slim ships without the system
	// timezone database, and TZ selects how cron expressions are interpreted.
	_ "time/tzdata"

	"github.com/go-co-op/gocron/v2"
	"github.com/joho/godotenv"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// A missing .env is normal in production, where Easypanel injects real
	// environment variables.
	_ = godotenv.Load()

	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}

	installed, err := Discover(cfg.PGBinDir)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("pg_dump available for majors %v", installed.Majors())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := NewStore(cfg)

	// One backup at a time process-wide, queued rather than dropped.
	// Independent tiers can legitimately come due at the same instant, for
	// example daily and weekly both at 22:00 on a Sunday, and concurrent
	// dumps would put avoidable load on the database.
	scheduler, err := gocron.NewScheduler(
		gocron.WithLocation(cfg.Location),
		gocron.WithLimitConcurrentJobs(1, gocron.LimitModeWait),
	)
	if err != nil {
		log.Fatalf("creating scheduler: %v", err)
	}

	for _, tier := range cfg.Tiers {
		tier := tier
		_, err := scheduler.NewJob(
			gocron.CronJob(tier.Cron, false),
			gocron.NewTask(func() { runTier(ctx, cfg, installed, store, tier) }),
		)
		if err != nil {
			log.Fatalf("tier %s: invalid cron expression %q: %v", tier.Name, tier.Cron, err)
		}
		log.Printf("tier %s scheduled %q keeping %d days", tier.Name, tier.Cron, tier.KeepDays)
	}

	scheduler.Start()
	log.Printf("pg_backup_s3 running for %d database(s) in %s", len(cfg.Databases), cfg.Location)

	<-ctx.Done()
	log.Printf("shutting down")
	if err := scheduler.Shutdown(); err != nil {
		log.Printf("scheduler shutdown: %v", err)
	}
}

// runTier backs up every configured database in sequence. A failure on one
// database is logged and the next one is still attempted, so one broken
// database cannot stop the others or bring down the scheduler.
func runTier(ctx context.Context, cfg *Config, installed Installed, store *Store, tier Tier) {
	log.Printf("[%s] starting", tier.Name)
	start := time.Now()

	for _, db := range cfg.Databases {
		if ctx.Err() != nil {
			log.Printf("[%s] cancelled", tier.Name)
			return
		}
		if err := backupDatabase(ctx, cfg, installed, store, tier, db); err != nil {
			log.Printf("[%s] %s ERROR: %v", tier.Name, db, err)
		}
	}

	log.Printf("[%s] finished in %s", tier.Name, time.Since(start).Round(time.Second))
}

// backupDatabase dumps one database straight into S3, then prunes that tier.
//
// Ordering is deliberate. The upload must complete before Wait is called,
// because pg_dump blocks once the pipe fills, which means upload success is
// known before dump success. A dump that dies part-way still produces a
// perfectly valid but truncated object, so the exit status is checked and the
// object deleted before pruning is ever considered.
func backupDatabase(ctx context.Context, cfg *Config, installed Installed, store *Store, tier Tier, db string) error {
	env := cfg.PGEnv(db)

	major, err := DetectMajor(ctx, installed.Newest(), env)
	if err != nil {
		return fmt.Errorf("detecting server version: %w", err)
	}
	binDir, err := installed.Pick(major)
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf("db_backup/%s/%s/", db, tier.Name)
	key := fmt.Sprintf("%s%s-%s.dump", prefix, db, time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	dump, err := StartDump(ctx, binDir, env)
	if err != nil {
		return err
	}

	if upErr := store.Upload(ctx, key, dump.Stdout); upErr != nil {
		// pg_dump would block forever writing into a pipe nobody reads.
		dump.Terminate()
		_ = dump.Wait()
		_ = store.Delete(ctx, key)
		return upErr
	}

	if err := dump.Wait(); err != nil {
		if delErr := store.Delete(ctx, key); delErr != nil {
			log.Printf("[%s] %s WARNING: could not remove failed dump %s: %v",
				tier.Name, db, key, delErr)
		}
		return fmt.Errorf("pg_dump against PostgreSQL %d: %w", major, err)
	}

	log.Printf("[%s] %s uploaded %s (pg_dump %d)", tier.Name, db, key, major)

	cutoff := time.Now().Add(-time.Duration(tier.KeepDays) * 24 * time.Hour)
	deleted, err := store.Prune(ctx, prefix, cutoff, key)
	if err != nil {
		return fmt.Errorf("pruning: %w", err)
	}
	if deleted > 0 {
		log.Printf("[%s] %s pruned %d backup(s) older than %d days", tier.Name, db, deleted, tier.KeepDays)
	}
	return nil
}
```

- [ ] **Step 3: Create `.env.example`**

```bash
# Object storage
S3_REGION=us-east-1
S3_ACCESS_KEY_ID=
S3_SECRET_ACCESS_KEY=
S3_BUCKET=
S3_ENDPOINT=https://s3.example.com
# Set true only for providers that require path-style addressing (e.g. MinIO)
# S3_FORCE_PATH_STYLE=false

# Database. POSTGRES_DATABASE is a comma-separated list.
POSTGRES_HOST=
POSTGRES_PORT=5432
POSTGRES_USER=postgres
POSTGRES_PASSWORD=
POSTGRES_DATABASE=app

# Retention tiers. A tier runs only when BOTH of its variables are set,
# so any tier may be omitted entirely, including hourly.
# SCHEDULE_HOURLY=0 * * * *
# BACKUP_KEEP_DAYS_HOURLY=2

SCHEDULE_DAILY=0 22 * * *
BACKUP_KEEP_DAYS_DAILY=7

SCHEDULE_WEEKLY=0 22 * * 0
BACKUP_KEEP_DAYS_WEEKLY=30

SCHEDULE_MONTHLY=0 22 1 * *
BACKUP_KEEP_DAYS_MONTHLY=365

# Timezone used to interpret the cron expressions above. Defaults to UTC.
TZ=UTC
```

- [ ] **Step 4: Verify the binary builds and every test passes**

Run: `go build ./... && go test ./... -v && gofmt -l . && go vet ./...`
Expected: build succeeds, all tests PASS, no output from gofmt or vet

- [ ] **Step 5: Verify configuration validation works end to end**

Run: `go run . 2>&1 | head -20`
Expected: exits non-zero with `invalid configuration:` followed by a bulleted
list of every missing variable. It must NOT panic or report only the first
problem.

- [ ] **Step 6: Commit**

```bash
git add main.go .env.example go.mod go.sum
git commit -m "feat: wire scheduler with one independent job per tier

Replaces the single hourly cron and its getHours()==22 tier checks.
Errors are contained per database so a failure no longer takes down the
daemon, and prune runs only after a verified-good dump."
```

---

### Task 7: Docker packaging

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `docker-compose.yml`
- Delete: `nixpacks.toml`

**Interfaces:**
- Consumes: the `pg_backup_s3` binary from Task 6
- Produces: a runnable image with `pg_dump` majors 14–18 at `/usr/lib/postgresql/<major>/bin`

- [ ] **Step 1: Create `.dockerignore`**

```
.git
.gitignore
.env
.env.*
!.env.example
docs/
*_test.go
docker-compose.yml
Dockerfile
README.md
```

- [ ] **Step 2: Create `Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pg_backup_s3 .

FROM debian:bookworm-slim

# Client majors to bundle. PostgreSQL 13 reached end of life in November 2025.
# Override at build time to add a new release: --build-arg PG_MAJORS="14 15 16 17 18 19"
ARG PG_MAJORS="14 15 16 17 18"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; \
    for v in $PG_MAJORS; do \
        apt-get install -y --no-install-recommends "postgresql-client-$v"; \
    done; \
    apt-get purge -y curl; \
    apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /out/pg_backup_s3 /usr/local/bin/pg_backup_s3

# Nothing is written to disk, so the container needs no writable paths.
USER nobody

ENTRYPOINT ["/usr/local/bin/pg_backup_s3"]
```

- [ ] **Step 3: Create `docker-compose.yml`**

This is for local verification and as a reference for the required
environment. Production deployment uses Easypanel, not this file.

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: devpassword
      POSTGRES_DB: app
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      timeout: 3s
      retries: 15

  minio:
    image: minio/minio
    command: server /data
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 2s
      timeout: 3s
      retries: 15

  # Creates the bucket, then stays alive so verification steps can run mc
  # commands against the stack with `docker compose exec mc ...`.
  mc:
    image: minio/mc
    depends_on:
      minio:
        condition: service_healthy
    entrypoint:
      - sh
      - -c
      - mc alias set local http://minio:9000 minioadmin minioadmin &&
        mc mb --ignore-existing local/backups &&
        sleep infinity
    healthcheck:
      test: ["CMD", "mc", "ls", "local/backups"]
      interval: 2s
      timeout: 3s
      retries: 15

  backup:
    build: .
    depends_on:
      postgres:
        condition: service_healthy
      mc:
        condition: service_healthy
    environment:
      S3_REGION: us-east-1
      S3_ACCESS_KEY_ID: minioadmin
      S3_SECRET_ACCESS_KEY: minioadmin
      S3_BUCKET: backups
      S3_ENDPOINT: http://minio:9000
      S3_FORCE_PATH_STYLE: "true"
      POSTGRES_HOST: postgres
      POSTGRES_PORT: "5432"
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: devpassword
      POSTGRES_DATABASE: app
      SCHEDULE_HOURLY: "* * * * *"
      BACKUP_KEEP_DAYS_HOURLY: "1"
      TZ: UTC
```

- [ ] **Step 4: Delete the nixpacks configuration**

```bash
git rm nixpacks.toml
```

- [ ] **Step 5: Build the image and verify the client binaries landed**

```bash
docker build -t pg_backup_s3:test .
docker run --rm --entrypoint sh pg_backup_s3:test -c 'ls /usr/lib/postgresql'
```

Expected: the listing shows exactly `14 15 16 17 18`.

- [ ] **Step 6: Verify the binary starts and reports its discovered clients**

```bash
docker run --rm pg_backup_s3:test 2>&1 | head -20
```

Expected: exits non-zero with the `invalid configuration:` list, proving the
binary runs as `nobody` and reaches configuration validation.

- [ ] **Step 7: Commit**

```bash
git add Dockerfile .dockerignore docker-compose.yml
git commit -m "build: replace nixpacks with a multi-stage Dockerfile

Bundles pg_dump majors 14-18 from PGDG at the standard Debian paths, so
the server version detected at runtime can select a matching client."
```

---

### Task 8: End-to-end verification and removal of the Node implementation

**Files:**
- Delete: `src/index.ts`, `src/dumpFile.ts`, `src/env.ts`, `src/s3.ts`, `src/removeFile.ts`
- Delete: `package.json`, `tsconfig.json`, `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`
- Create: `README.md`

**Interfaces:**
- Consumes: the image from Task 7
- Produces: a repository containing only the Go implementation

- [ ] **Step 1: Run the full stack and confirm a backup lands**

```bash
docker compose up --build -d
sleep 90
docker compose logs backup
```

Expected: a log line matching `[hourly] app uploaded db_backup/app/hourly/app-<timestamp>.dump (pg_dump 16)`.
The detected major must be `16`, proving version detection picked the client
matching the `postgres:16` server rather than the newest installed.

- [ ] **Step 2: Confirm exactly one object landed**

```bash
docker compose exec -T mc mc ls --recursive local/backups
```

Expected: exactly one `.dump` object under `db_backup/app/hourly/`, with a
non-zero size.

- [ ] **Step 3: Confirm the streamed bytes are an intact archive**

```bash
KEY=$(docker compose exec -T mc mc ls --recursive local/backups | awk '{print $NF}' | head -1)
docker compose exec -T mc mc cat "local/backups/$KEY" > /tmp/verify.dump
docker run --rm -v /tmp/verify.dump:/verify.dump:ro \
  --entrypoint /usr/lib/postgresql/16/bin/pg_restore \
  pg_backup_s3:test --list /verify.dump | head
```

Expected: a table of contents listing archive entries, not a format error.
This is the check that proves streaming to S3 produced a restorable archive
rather than merely a non-empty object.

- [ ] **Step 4: Confirm a failed dump leaves no object behind**

```bash
docker compose run -d --name failcheck -e POSTGRES_DATABASE=does_not_exist backup
sleep 75
docker logs failcheck | grep -i ERROR
docker compose exec -T mc mc ls --recursive local/backups
docker rm -f failcheck
```

Expected: the `docker logs` grep prints an `ERROR` line naming the connection
failure for `does_not_exist`, and the bucket listing still shows **only** the
`app` object from Step 2 — nothing under `db_backup/does_not_exist/`. This
verifies the delete-on-failure path and that a failed dump never counts as a
backup.

- [ ] **Step 5: Tear down**

```bash
docker compose down -v
rm -f /tmp/verify.dump
```

- [ ] **Step 6: Remove the TypeScript implementation**

`pnpm-lock.yaml` is untracked, so it is deleted from disk rather than with
`git rm`.

```bash
git rm -r src
git rm package.json tsconfig.json package-lock.json yarn.lock
rm -f pnpm-lock.yaml
rm -rf node_modules dist
```

- [ ] **Step 7: Create `README.md`**

````markdown
# pg_backup_s3

Scheduled PostgreSQL backups streamed straight to S3-compatible storage.

- Detects each server's version at runtime and runs a matching `pg_dump`
  (majors 14–18 bundled)
- Streams `pg_dump --format=custom` output directly to S3; nothing is ever
  written to local disk, so memory stays bounded regardless of database size
- Independent retention tiers — hourly, daily, weekly, monthly — each of
  which may be omitted
- A failed dump never rotates away good backups

## Configuration

Copy `.env.example` to `.env` for local use. In production, set these as
environment variables on the service.

A tier runs only when **both** `SCHEDULE_<TIER>` and
`BACKUP_KEEP_DAYS_<TIER>` are set. Setting just one is a startup error, so a
typo cannot silently disable backups. At least one tier is required.

`POSTGRES_DATABASE` is a comma-separated list; each database is backed up in
sequence.

Cron expressions use five fields and are interpreted in `TZ`, which defaults
to `UTC`.

## Deploying on Easypanel

1. Create an **App** service pointing at this repository
2. Set the build method to **Dockerfile**
3. Paste the environment variables from `.env.example`, filled in
4. Deploy

No volumes are required — the container writes nothing to disk.

To add a PostgreSQL major that is not bundled, set the build argument:

```
PG_MAJORS=14 15 16 17 18 19
```

## Object layout

```
db_backup/<database>/<tier>/<database>-<timestamp>.dump
```

## Restoring

```bash
pg_restore --clean --if-exists -d <database> <file>.dump
```

Use a `pg_restore` at least as new as the `pg_dump` that produced the file.
The log line for each backup records which major was used.

## Local development

```bash
go test ./...
docker compose up --build
```

The compose stack runs PostgreSQL and MinIO and takes a backup every minute.
````

- [ ] **Step 8: Verify the repository is clean and Go-only**

```bash
go build ./... && go test ./...
git status --short
ls
```

Expected: build and tests pass. No `src/`, `package.json`, `tsconfig.json`,
or any lockfile remains. The listing shows only Go sources, `Dockerfile`,
`docker-compose.yml`, `.dockerignore`, `.env.example`, `README.md`, `docs/`,
`go.mod`, and `go.sum`.

- [ ] **Step 9: Commit**

```bash
git add -A
git commit -m "refactor!: remove the TypeScript implementation

The Go rewrite replaces it entirely. Also drops three redundant lockfiles
(package-lock.json, yarn.lock, pnpm-lock.yaml) that had accumulated.

BREAKING CHANGE: the Easypanel service must switch its build method from
Nixpacks to Dockerfile. Dumps are now custom format (.dump) rather than
plain gzip (.gz); existing .gz objects age out through normal retention."
```

---

## Verification Summary

After Task 8 the following must all hold:

| Requirement | Verified by |
|---|---|
| Refactor into clear units | Four focused files, each with one responsibility |
| Cleanup | Task 8 Step 7 — no Node artifacts, one dependency manifest |
| Best performance | Streaming upload, bounded memory, no disk; Task 8 Step 2 proves the stream is intact |
| Multiple PostgreSQL versions | Task 2 tests; Task 8 Step 1 shows major 16 selected against a 16 server |
| Hourly not required | Task 1 test `hourly is not required`; `.env.example` ships hourly commented out |
| Easy Docker deploy | Task 7 Steps 5–6; README Easypanel section |
| Failed dump cannot rotate good backups | Task 8 Step 3 |
| Retention works past 1000 objects | Task 4 `TestChunk`; paginator in Task 5 |

## Known Limitations

- PostgreSQL 9.x servers are rejected by `parseServerVersion`. Those releases
  used a different version-numbering scheme and have been end-of-life since
  2021. Supporting them would mean parsing a second format for no live use.
- Each tier takes its own dump rather than promoting one dump across tiers.
  Tiers fire on independent schedules, so there is not always a recent dump to
  copy from.
- Failures are logged only; there is no alerting. If silent failure becomes a
  concern, add an optional `HEALTHCHECK_URL` pinged on success and on failure.
