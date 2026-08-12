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
