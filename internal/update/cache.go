package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CacheFile is the update-check state file, beside profiles.json and
// the persisted timezone choice in app.Paths.StateDir (the repo's
// established convention for durable single-value state; see
// internal/profiles/profiles.go DefaultPath's doc comment).
const CacheFile = "update-check.json"

const cacheSchema = 1

// checkInterval caps the background check at once per day — the
// industry norm (gh, pip, update-notifier) for notify-only updaters.
const checkInterval = 24 * time.Hour

// Cache stores FACTS only (when we checked, what the registry said) —
// never a "newer" verdict. Whether to notify is recomputed at render
// time against the running version, so a cache written before an
// upgrade goes naturally quiet after it without cleanup.
type Cache struct {
	Schema    int       `json:"schema"`
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func cachePath(stateDir string) string { return filepath.Join(stateDir, CacheFile) }

// LoadCache returns the cache and whether it is usable. Any defect —
// missing file, corrupt JSON, unknown schema, zero time, blank latest —
// reads as "no cache": the file is disposable by contract, so a bad one
// must degrade to a silent re-check, never to an error.
func LoadCache(stateDir string) (Cache, bool) {
	b, err := os.ReadFile(cachePath(stateDir))
	if err != nil {
		return Cache{}, false
	}
	var c Cache
	if json.Unmarshal(b, &c) != nil || c.Schema != cacheSchema ||
		c.CheckedAt.IsZero() || strings.TrimSpace(c.Latest) == "" {
		return Cache{}, false
	}
	return c, true
}

// SaveCache writes atomically (same-dir temp file + rename) so a
// concurrent reader never sees a torn file. Two sessions checking at
// once is a benign last-write-wins race — both write equivalent facts
// seconds apart — which is why this is a plain rename and not an flock
// like profiles.json (that one is read-modify-write user data).
func SaveCache(stateDir string, c Cache) error {
	c.Schema = cacheSchema
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".update-check-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmpName, cachePath(stateDir)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Due reports whether a new network check is allowed at `now`.
func Due(c Cache, ok bool, now time.Time) bool {
	if !ok {
		return true
	}
	return now.Sub(c.CheckedAt) >= checkInterval
}
