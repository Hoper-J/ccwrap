package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func Path(sessionDir string) string {
	return filepath.Join(sessionDir, "manifest.json")
}

func Write(path string, m model.SessionManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Read(path string) (model.SessionManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.SessionManifest{}, err
	}
	var m model.SessionManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return model.SessionManifest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}
