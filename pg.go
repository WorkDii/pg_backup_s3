package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
