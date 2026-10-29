package uiservice

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// When others can write to the runtime dir, the ui trusts nothing in it and
// creates nothing in it: Running ignores the ui.json there, and neither
// Ensure (ccwrap open) nor Serve (ccwrap ui serve) uses the directory.
func TestUIRefusesRuntimeDirOthersCanWrite(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	startUI(t, paths, nil, time.Hour)
	if err := os.Chmod(paths.RuntimeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(paths.RuntimeDir, 0o700) })

	if _, ok := Running(context.Background(), paths); ok {
		t.Fatal("Running trusted the ui.json in a runtime dir others can write to")
	}
	if _, err := Ensure(context.Background(), paths, "/nonexistent/ccwrap", "test", time.Second); err == nil || !strings.Contains(err.Error(), "refusing runtime directory") {
		t.Fatalf("Ensure = %v, want a refusal", err)
	}
	srv := New(Options{Paths: paths, Version: "test", Exe: "/usr/local/bin/ccwrap", Getenv: func(string) string { return "" }, Logger: log.New(io.Discard, "", 0)})
	if err := srv.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing runtime directory") {
		t.Fatalf("Serve = %v, want a refusal", err)
	}
}
