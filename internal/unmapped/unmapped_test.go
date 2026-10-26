package unmapped

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRecordAndModelsPerProfile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for _, r := range []struct{ profile, model string }{
		{"deepseek", "claude-opus-5-5"}, {"deepseek", "claude-haiku-4-5"}, {"deepseek", "claude-opus-5-5"}, {"enjoy", "claude-fable-5-1"},
	} {
		if err := Record(dir, r.profile, r.model, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := Models(dir, "deepseek"); !reflect.DeepEqual(got, []string{"claude-haiku-4-5", "claude-opus-5-5"}) {
		t.Fatalf("deepseek = %v", got)
	}
	if got := Models(dir, "enjoy"); !reflect.DeepEqual(got, []string{"claude-fable-5-1"}) {
		t.Fatalf("enjoy = %v", got)
	}
	if got := Models(dir, "other"); len(got) != 0 {
		t.Fatalf("other = %v", got)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
	// Nothing to record without a profile or a model.
	if err := Record(dir, "", "claude-x", now); err != nil || len(Models(dir, "")) != 0 {
		t.Fatal("an empty profile is not recorded")
	}
}

func TestRecordKeepsTheNewestPerProfile(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i := 0; i < maxPerProfile+4; i++ {
		if err := Record(dir, "p", fmt.Sprintf("claude-m-%02d", i), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got := Models(dir, "p")
	if len(got) != maxPerProfile || got[0] != "claude-m-04" {
		t.Fatalf("kept %d, first %q", len(got), got[0])
	}
}

// A corrupt file reads as empty and is replaced on the next record.
func TestCorruptFileStartsOver(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(Models(dir, "p")) != 0 {
		t.Fatal("corrupt file must read as empty")
	}
	if err := Record(dir, "p", "claude-x", time.Now()); err != nil || !reflect.DeepEqual(Models(dir, "p"), []string{"claude-x"}) {
		t.Fatalf("record after corruption: %v %v", err, Models(dir, "p"))
	}
}
