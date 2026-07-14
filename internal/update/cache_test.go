package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Cache{CheckedAt: time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC), Latest: "0.3.1"}
	if err := SaveCache(dir, in); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	out, ok := LoadCache(dir)
	if !ok {
		t.Fatal("LoadCache: expected ok")
	}
	if !out.CheckedAt.Equal(in.CheckedAt) || out.Latest != "0.3.1" || out.Schema != 1 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	// The atomic write must leave no temp-file residue.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly the cache file, got %d entries", len(entries))
	}
}

func TestLoadCacheTolerance(t *testing.T) {
	// Missing file, corrupt JSON, unknown schema, empty latest — all
	// treated as no cache: the cache can always be deleted by hand or
	// mangled by an older version without breaking anything.
	dir := t.TempDir()
	if _, ok := LoadCache(dir); ok {
		t.Fatal("missing file should be not-ok")
	}
	path := filepath.Join(dir, CacheFile)
	for name, content := range map[string]string{
		"corrupt json":   "{not json",
		"unknown schema": `{"schema":99,"checked_at":"2026-07-13T08:00:00Z","latest":"0.3.1"}`,
		"empty latest":   `{"schema":1,"checked_at":"2026-07-13T08:00:00Z","latest":""}`,
		"zero time":      `{"schema":1,"latest":"0.3.1"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := LoadCache(dir); ok {
			t.Fatalf("%s should be not-ok", name)
		}
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if !Due(Cache{}, false, now) {
		t.Fatal("no cache should be due")
	}
	fresh := Cache{Schema: 1, CheckedAt: now.Add(-time.Hour), Latest: "0.3.1"}
	if Due(fresh, true, now) {
		t.Fatal("1h-old cache should NOT be due")
	}
	stale := Cache{Schema: 1, CheckedAt: now.Add(-25 * time.Hour), Latest: "0.3.1"}
	if !Due(stale, true, now) {
		t.Fatal("25h-old cache should be due")
	}
	// The boundary is pinned at >=: exactly 24h must be due (otherwise a
	// user who launches exactly once a day is forever one second short).
	edge := Cache{Schema: 1, CheckedAt: now.Add(-24 * time.Hour), Latest: "0.3.1"}
	if !Due(edge, true, now) {
		t.Fatal("exactly-24h-old cache should be due (boundary is >=)")
	}
}
