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
