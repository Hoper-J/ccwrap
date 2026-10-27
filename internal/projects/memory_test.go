package projects

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRememberLaunch(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := RememberLaunch(dir, "/w/a", "deepseek", now); err != nil {
		t.Fatal(err)
	}
	if err := RememberLaunch(dir, "/w/b", "", now); err != nil {
		t.Fatal(err)
	}
	if err := RememberLaunch(dir, "/w/a", "official", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	m := Memories(dir)
	if m["/w/a"].LastProfile != "official" || m["/w/b"].LastProfile != "" || len(m) != 2 {
		t.Fatalf("memories = %+v", m)
	}
	if info, err := os.Stat(filepath.Join(dir, memoryFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	if len(Memories(t.TempDir())) != 0 {
		t.Fatal("no file reads as empty")
	}
}

func TestRememberLaunchKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i := 0; i < maxMemories+3; i++ {
		if err := RememberLaunch(dir, fmt.Sprintf("/w/%04d", i), "p", base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	m := Memories(dir)
	if _, ok := m["/w/0000"]; ok || len(m) != maxMemories {
		t.Fatalf("kept %d, oldest still there: %v", len(m), ok)
	}
}
