package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// OverwriteFile validates f and atomically replaces path with the
// marshaled JSON.
func OverwriteFile(path string, f *File, sourceLabel string) error {
	if f == nil {
		return fmt.Errorf("write profiles: nil file")
	}
	if err := Validate(f, sourceLabel); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profiles: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir profiles dir: %w", err)
	}
	fd, err := os.CreateTemp(dir, ".profiles.json.*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := fd.Name()
	// Defensive chmod matches the repo's atomic writers (certs/ca.go,
	// cmd/ccwrap/main.go). os.CreateTemp already returns 0o600 today,
	// but the explicit chmod self-documents the contract.
	if err := fd.Chmod(0o600); err != nil {
		_ = fd.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if _, err := fd.Write(blob); err != nil {
		_ = fd.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := fd.Sync(); err != nil {
		_ = fd.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename tmp -> profiles.json: %w", err)
	}
	return nil
}
