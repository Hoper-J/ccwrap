// Package statefile reads and writes ccwrap's small JSON state files. Writes
// are atomic (temp file + rename) with mode 0600; reads treat a missing or
// corrupt file as empty, since these files only carry hints.
package statefile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Read decodes dir/name into v. A missing or unreadable file leaves v as is.
func Read(dir, name string, v any) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err == nil {
		_ = json.Unmarshal(data, v)
	}
}

// Write encodes v into dir/name atomically.
func Write(dir, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
