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
