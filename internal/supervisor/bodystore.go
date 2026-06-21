package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// bodyStore spills captured request bodies to <dir>/bodies/<id>.json
// (0600, dir 0700) via an async writer so the proxy forward never blocks
// on disk. The 250-ring keeps only model.RequestBodyRef.
type bodyStore struct {
	dir         string // == per-session RuntimeDir
	budgetBytes int64
	mu          sync.Mutex
	wg          sync.WaitGroup
	closed      bool // terminal; set by removeAll under s.mu, never reset
}

func newBodyStore(dir string, budgetBytes int64) *bodyStore {
	return &bodyStore{dir: dir, budgetBytes: budgetBytes}
}

func (s *bodyStore) bodiesDir() string { return filepath.Join(s.dir, "bodies") }

// put computes the ref synchronously (cheap; sha over the in-hand buffer)
// and writes the file asynchronously so the forward is never blocked.
func (s *bodyStore) put(id string, payload []byte) *model.RequestBodyRef {
	sum := sha256.Sum256(payload)
	ref := &model.RequestBodyRef{
		ID:         id,
		Size:       int64(len(payload)),
		SHA256:     hex.EncodeToString(sum[:]),
		CapturedAt: time.Now().UTC(),
	}
	buf := make([]byte, len(payload)) // own copy; caller may reuse/restore
	copy(buf, payload)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			// Session ended (removeAll ran): suppress a late write so we
			// never recreate bodies/<id>.json past session end. The caller
			// already holds the ref; the missing file yields the
			// "missing → endpoint 404" behavior, correct for a closing
			// session (privacy: no orphaned cleartext body).
			return
		}
		if err := os.MkdirAll(s.bodiesDir(), 0o700); err != nil {
			return // write-failed: ref stays, endpoint will 404
		}
		// Atomic publish: write to a sibling .tmp then rename into place. A
		// concurrent reader (load → /recent/body) then sees either no file or
		// the COMPLETE file — never the empty/partial window os.WriteFile would
		// expose between create-truncate and write. The .tmp suffix is ignored
		// by enforceBudgetLocked (.json-only) and wiped by removeAll's RemoveAll.
		final := filepath.Join(s.bodiesDir(), id+".json")
		tmp := final + ".tmp"
		if err := os.WriteFile(tmp, buf, 0o600); err != nil {
			return // write-failed: ref stays, endpoint will 404
		}
		if err := os.Rename(tmp, final); err != nil {
			_ = os.Remove(tmp) // don't leave a stray .tmp on rename failure
			return
		}
		s.enforceBudgetLocked()
	}()
	return ref
}

// load reads a spilled body by id. The guard is defense-in-depth against
// path traversal: filepath.Join lexically cleans ".."
// WITHOUT containment, so without this an id with a separator or ".."
// would escape bodiesDir() and read an arbitrary *.json (including
// other sessions' cleartext bodies). id != filepath.Base(id) rejects
// any id carrying a path separator ("a/b", "/abs", "../x" all fail);
// the strings.Contains(id, "..") is extra belt. Callers MUST also
// validate (see isBodyID at the endpoint) — this makes load safe
// regardless of caller.
func (s *bodyStore) load(id string) ([]byte, error) {
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return nil, fmt.Errorf("bodystore: invalid body id")
	}
	return os.ReadFile(filepath.Join(s.bodiesDir(), id+".json"))
}

func (s *bodyStore) delete(id string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return // removeAll already wiped everything; nothing to remove
		}
		_ = os.Remove(filepath.Join(s.bodiesDir(), id+".json"))
	}()
}

// removeAll is the terminal/idempotent session-end shred: it drops
// the whole per-session bodies/ dir and guarantees no put/delete goroutine
// can (re)create a file afterwards.
//
// Ordering is load-bearing. We set closed=true under s.mu *before*
// wg.Wait(): any put/delete goroutine that acquires s.mu after this point
// observes closed and no-ops (see the closed checks in put/delete). After
// wg.Wait() every already-issued goroutine has therefore either (a)
// completed its write before closed was set — the final RemoveAll wipes it
// — or (b) reached its critical section after closed=true and no-op'd. So
// once removeAll returns, no body file or bodies/ dir can be recreated.
// Re-calling removeAll is a no-op (closed stays true; os.RemoveAll is nil
// on a missing dir).
func (s *bodyStore) removeAll() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wg.Wait() // let already-issued put/delete goroutines run; they no-op if they reach their critical section after closed=true, or completed their write before it
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.RemoveAll(s.bodiesDir())
}

// enforceBudgetLocked deletes oldest files until under budget.
// Caller holds s.mu.
func (s *bodyStore) enforceBudgetLocked() {
	if s.budgetBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(s.bodiesDir())
	if err != nil {
		return
	}
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fi
	var total int64
	for _, e := range entries {
		// Budget accounting & eviction cover ONLY <id>.json body files.
		// put is the sole writer and only ever emits *.json, so this is a
		// defensive invariant today — but it keeps any future sidecar/tmp
		// file in bodies/ from inflating `total` (premature eviction of
		// real bodies) or being deleted as if it were an evictable body
		// (privacy layer: never under-evict cleartext bodies).
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{filepath.Join(s.bodiesDir(), e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= s.budgetBytes {
			break
		}
		_ = os.Remove(f.path)
		total -= f.size
	}
}
