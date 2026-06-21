package upstreamheaders

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFromEnvFileAndPairs(t *testing.T) {
	cfg, _, err := Resolve(ResolveOptions{
		ParentEnv:     []string{`CCWRAP_UPSTREAM_HEADERS_JSON={"X-Gateway-Tenant":"team-a"}`},
		ExplicitPairs: []string{"X-Gateway-Key=sentinel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headers["X-Gateway-Tenant"] != "team-a" || cfg.Headers["X-Gateway-Key"] != "sentinel" {
		t.Fatalf("unexpected headers: %#v", cfg.Headers)
	}
	if cfg.Fingerprint == "" {
		t.Fatal("expected fingerprint")
	}
}

func TestRejectsInvalidHeaderNameAndNewline(t *testing.T) {
	if _, err := ParsePairs([]string{"Bad Header=value"}); err == nil {
		t.Fatal("expected invalid header name to be rejected")
	}
	if _, err := ParsePairs([]string{"X-Test=value\nnext"}); err == nil {
		t.Fatal("expected newline header value to be rejected")
	}
}

func TestResolveUpstreamExplicitFileContentWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "headers.json")
	if err := os.WriteFile(path, []byte(`{"X-On-Disk":"odv"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	cfg, _, err := Resolve(ResolveOptions{
		ExplicitFile:        path,
		ExplicitFileContent: []byte(`{"X-Snapshot":"snv"}`),
	})
	if err != nil {
		t.Fatalf("Resolve err: %v", err)
	}
	if cfg.Headers["X-Snapshot"] != "snv" {
		t.Errorf("Headers[X-Snapshot] = %q, want snv", cfg.Headers["X-Snapshot"])
	}
	if _, present := cfg.Headers["X-On-Disk"]; present {
		t.Errorf("on-disk file leaked; Headers = %v", cfg.Headers)
	}
}

func TestResolveUpstreamEnvFileContentBypassesEnvPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env-headers.json")
	if err := os.WriteFile(envPath, []byte(`{"X-Env":"ev"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	cfg, _, err := Resolve(ResolveOptions{
		ParentEnv:      []string{EnvHeadersFile + "=" + envPath},
		EnvFileContent: []byte(`{"X-Envsnap":"esv"}`),
	})
	if err != nil {
		t.Fatalf("Resolve err: %v", err)
	}
	// Note: net/http canonicalization downcases letters after the first cap in each hyphen-segment,
	// so X-EnvSnap → X-Envsnap. We assert on the canonical key the package stores.
	if cfg.Headers["X-Envsnap"] != "esv" {
		t.Errorf("Headers[X-Envsnap] = %q, want esv", cfg.Headers["X-Envsnap"])
	}
	if _, present := cfg.Headers["X-Env"]; present {
		t.Errorf("env-tier disk file was read; Headers = %v", cfg.Headers)
	}
}

func TestResolveUpstreamBackcompatNilContentFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "headers.json")
	if err := os.WriteFile(path, []byte(`{"X-K":"v"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	cfg, _, err := Resolve(ResolveOptions{ExplicitFile: path})
	if err != nil {
		t.Fatalf("Resolve err: %v", err)
	}
	if cfg.Headers["X-K"] != "v" {
		t.Errorf("Headers[X-K] = %q, want v", cfg.Headers["X-K"])
	}
}
