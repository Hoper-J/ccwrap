package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestBodyStoreWriteLoadDelete(t *testing.T) {
	dir := t.TempDir()
	st := newBodyStore(dir, 256*1024*1024)
	payload := []byte(`{"hello":"world"}`)
	ref := st.put("req-1", payload)
	if ref == nil || ref.ID != "req-1" || ref.Size != int64(len(payload)) {
		t.Fatalf("put returned bad ref: %+v", ref)
	}
	sum := sha256.Sum256(payload)
	if ref.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha mismatch")
	}
	path := filepath.Join(dir, "bodies", "req-1.json")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && string(b) == string(payload) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := st.load("req-1")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("load: %v %q", err, got)
	}
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file must be 0600, got %v", fi.Mode())
	}
	st.delete("req-1")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delete did not remove file")
}

// TestBodyStorePutPublishesAtomicallyNoTmpLingers locks the atomic temp+rename
// publish: after the async write settles, the final <id>.json holds the content
// and NO sibling .tmp file is left behind (a stray .tmp would mean rename failed
// or cleanup regressed, and would also escape the .json-only budget accounting).
func TestBodyStorePutPublishesAtomicallyNoTmpLingers(t *testing.T) {
	dir := t.TempDir()
	st := newBodyStore(dir, 256*1024*1024)
	payload := []byte(`{"atomic":"publish"}`)
	st.put("req-atomic", payload)

	bodiesDir := filepath.Join(dir, "bodies")
	final := filepath.Join(bodiesDir, "req-atomic.json")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(final); err == nil && string(b) == string(payload) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, err := os.ReadFile(final); err != nil || string(b) != string(payload) {
		t.Fatalf("final file = %q err=%v, want %q", b, err, payload)
	}
	entries, err := os.ReadDir(bodiesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
}

// TestBodyStorePutAfterRemoveAllSuppressed is the privacy regression for the
// put/delete-after-removeAll orphan window (session-end wipes the whole
// bodies/ dir with an aggressive shred). removeAll sets the terminal
// `closed` flag synchronously *before* it returns, so any put issued after
// removeAll must no-op in its writer goroutine: no file is (re)created and
// the bodies/ dir stays gone. This is deterministic — closed=true is
// visible the instant removeAll returns, with no inter-goroutine race to
// observe. Without the closed flag the put goroutine would
// MkdirAll+WriteFile and recreate the orphan; this test fails there.
// TestEnforceBudgetCountsOnlyJSON is the privacy-layer hardening
// regression: budget accounting (enforceBudgetLocked) must count and evict
// ONLY <id>.json body files. put is the sole writer and only ever emits
// *.json today, so this is exact now — but a future sidecar/tmp file in
// bodies/ must not (a) inflate `total` and cause premature eviction of real
// bodies, nor (b) itself be deleted as if it were an evictable body.
//
// Deterministic by construction: enforceBudgetLocked is unexported and
// driven directly here (in-package, no put goroutine, no polling). We seed
// three .json bodies with strictly increasing mtimes plus one stray
// notes.txt, set a budget that fits exactly two .json files, then call
// enforceBudgetLocked under s.mu and assert: the oldest .json is evicted,
// the two newest .json survive, and notes.txt is untouched (neither counted
// toward the over-budget total nor deleted). A loop that counts/sorts
// notes.txt too would put it in the delete candidate set and (being the
// lexicographically/temporally arbitrary extra) skew `total`, so this
// fails there.
func TestEnforceBudgetCountsOnlyJSON(t *testing.T) {
	dir := t.TempDir()
	// Budget = 200 bytes; each .json below is 100 bytes -> exactly 2 fit.
	st := newBodyStore(dir, 200)
	bodiesDir := filepath.Join(dir, "bodies")
	if err := os.MkdirAll(bodiesDir, 0o700); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 100) // 100-byte .json bodies
	for i := range body {
		body[i] = 'a'
	}
	base := time.Now().Add(-time.Hour)
	jsonIDs := []string{"a-oldest", "b-middle", "c-newest"}
	for i, id := range jsonIDs {
		p := filepath.Join(bodiesDir, id+".json")
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		// Strictly increasing mtimes so eviction order is unambiguous
		// (oldest-first): a-oldest < b-middle < c-newest.
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// Stray non-.json sidecar, larger than the whole budget and "newest" by
	// mtime so a pre-fix loop would both count it (forcing extra eviction)
	// and, were it sorted in, be a delete candidate.
	notes := filepath.Join(bodiesDir, "notes.txt")
	if err := os.WriteFile(notes, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	notesMT := base.Add(10 * time.Minute)
	if err := os.Chtimes(notes, notesMT, notesMT); err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	st.enforceBudgetLocked()
	st.mu.Unlock()

	// Only the single oldest .json (a-oldest) must be evicted: 3*100=300 >
	// 200, drop oldest -> 200 <= 200, stop. The stray notes.txt's 4096
	// bytes must NOT have been in `total` (else 2+ .json would be evicted).
	if _, err := os.Stat(filepath.Join(bodiesDir, "a-oldest.json")); !os.IsNotExist(err) {
		t.Fatalf("oldest .json must be evicted, stat err = %v", err)
	}
	for _, surv := range []string{"b-middle.json", "c-newest.json"} {
		if _, err := os.Stat(filepath.Join(bodiesDir, surv)); err != nil {
			t.Fatalf("%s must survive (only oldest .json evicts under a 2-file budget): %v", surv, err)
		}
	}
	// The non-.json sidecar must be neither counted nor deleted.
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("notes.txt must NOT be deleted by budget enforcement: %v", err)
	}
}

func TestBodyStorePutAfterRemoveAllSuppressed(t *testing.T) {
	dir := t.TempDir()
	st := newBodyStore(dir, 256*1024*1024)
	bodiesDir := filepath.Join(dir, "bodies")

	// Seed one file + drain its writer so removeAll has real work and the
	// dir genuinely exists pre-wipe.
	st.put("early", []byte(`{"k":"v"}`))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(bodiesDir, "early.json")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	st.removeAll() // terminal: sets closed=true, waits, RemoveAll(bodies/)

	// Synchronous contract is unchanged: put still returns a non-nil ref.
	if ref := st.put("late", []byte(`{"late":true}`)); ref == nil || ref.ID != "late" {
		t.Fatalf("put after removeAll must still return a ref synchronously, got %+v", ref)
	}
	// ...but the writer goroutine must have no-op'd: load("late") errors and
	// the bodies/ dir is never recreated. Poll a bounded window so a
	// pre-fix late write (which WOULD recreate the file) is caught.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(bodiesDir); !os.IsNotExist(err) {
			t.Fatalf("bodies/ dir recreated after removeAll (orphan window): %v", err)
		}
		if _, err := st.load("late"); err == nil {
			t.Fatalf("late body file created after removeAll (orphan window)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := st.load("late"); err == nil {
		t.Fatalf("late body file present after removeAll (orphan window)")
	}
	if _, err := os.Stat(bodiesDir); !os.IsNotExist(err) {
		t.Fatalf("bodies/ dir present after removeAll, want gone: %v", err)
	}
}

// TestEvictBodyFiles_ReclaimsBothRefs — a captured request
// can spill TWO files — BodyRef (client-view body) and UpstreamBodyRef
// (post-modelalias-rewrite body). Ring-eviction in recordRequest used to
// delete only BodyRef, orphaning the upstream-body file on disk for the rest
// of the session (unbounded growth of cleartext-redacted spill files under
// capture+rewrite load). evictBodyFiles must reclaim BOTH.
func TestEvictBodyFiles_ReclaimsBothRefs(t *testing.T) {
	dir := t.TempDir()
	st := newBodyStore(dir, 256*1024*1024)

	clientRef := st.put("client-1", []byte(`{"client":"body"}`))
	upstreamRef := st.put("upstream-1", []byte(`{"upstream":"body"}`))
	clientPath := filepath.Join(dir, "bodies", "client-1.json")
	upstreamPath := filepath.Join(dir, "bodies", "upstream-1.json")

	// Wait for both async writes to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(clientPath)
		_, e2 := os.Stat(upstreamPath)
		if e1 == nil && e2 == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	evicted := model.RequestRecord{BodyRef: clientRef, UpstreamBodyRef: upstreamRef}
	evictBodyFiles(st, evicted)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(clientPath)
		_, e2 := os.Stat(upstreamPath)
		if os.IsNotExist(e1) && os.IsNotExist(e2) {
			return // both reclaimed
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, e1 := os.Stat(clientPath)
	_, e2 := os.Stat(upstreamPath)
	t.Fatalf("eviction did not reclaim both bodies: client exists=%v, upstream exists=%v (upstream orphan = the bug)",
		!os.IsNotExist(e1), !os.IsNotExist(e2))
}

// TestEvictBodyFilesDeletesResponseBodyRef — the telemetry-MITM path spills a
// THIRD body file, ResponseBodyRef (the captured upstream RESPONSE body). Like
// BodyRef and UpstreamBodyRef, it must be reclaimed on ring eviction or the
// response-body file orphans on disk for the rest of the session.
func TestEvictBodyFilesDeletesResponseBodyRef(t *testing.T) {
	dir := t.TempDir()
	st := newBodyStore(dir, 256*1024*1024)

	ref := st.put("response-1", []byte(`{"ok":true}`))
	respPath := filepath.Join(dir, "bodies", "response-1.json")

	// Wait for the async write to land, then confirm it loads.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(respPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := st.load("response-1"); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("response body must load before eviction: %v %q", err, got)
	}

	rec := model.RequestRecord{ResponseBodyRef: ref}
	evictBodyFiles(st, rec)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(respPath); os.IsNotExist(err) {
			return // reclaimed
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("eviction did not reclaim the response body (orphan = the bug)")
}
