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
