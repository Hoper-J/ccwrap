package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DefaultReleaseBase is where release artifacts live. Version DISCOVERY
// goes to npm (reachability), but artifacts only exist on GitHub
// releases — the same split install.sh lives with. Lockstep between the
// two sources is guaranteed by the tag-driven release pipeline.
const DefaultReleaseBase = "https://github.com/Hoper-J/ccwrap/releases/download"

// maxArchiveSize bounds the release download (binaries are ~15 MiB;
// half a GiB is generous headroom, anything more is a broken endpoint).
const maxArchiveSize = 512 << 20

// Apply downloads the release archive for version/goos/goarch, verifies
// its sha256 against the published checksums.txt (same rigor as
// install.sh — no checksum, no install), extracts the `ccwrap` entry,
// and atomically renames it over exePath via a same-directory temp
// file. On ANY failure the existing binary is left untouched and the
// temp file is removed. A permission failure surfaces as
// os.ErrPermission so the caller can print an actionable sudo /
// CCWRAP_BINDIR hint instead of a raw error.
func Apply(ctx context.Context, client *http.Client, releaseBase, version, goos, goarch, exePath string, out io.Writer) error {
	archiveName := fmt.Sprintf("ccwrap_%s_%s_%s.tar.gz", version, goos, goarch)
	base := fmt.Sprintf("%s/v%s", strings.TrimRight(releaseBase, "/"), version)
	fmt.Fprintf(out, "downloading v%s (%s/%s)…\n", version, goos, goarch)

	sums, err := fetchBytes(ctx, client, base+"/checksums.txt", 1<<20)
	if err != nil {
		return fmt.Errorf("fetch checksums.txt: %w", err)
	}
	want, ok := checksumFor(string(sums), archiveName)
	if !ok {
		return fmt.Errorf("no checksum for %s in checksums.txt", archiveName)
	}
	archive, err := fetchBytes(ctx, client, base+"/"+archiveName, maxArchiveSize)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", archiveName, err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s (want %s, got %s)", archiveName, want, got)
	}
	bin, err := extractBinary(archive)
	if err != nil {
		return err
	}

	dir := filepath.Dir(exePath)
	tmp := filepath.Join(dir, fmt.Sprintf(".ccwrap.new-%d", os.Getpid()))
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		// A half-written file (e.g. a mid-write ENOSPC failure) must
		// not leave residue either; when the file was never created,
		// Remove fails silently and is harmless. os.ErrPermission
		// semantics are unaffected.
		_ = os.Remove(tmp)
		return err // os.WriteFile already wraps ErrPermission for the caller's errors.Is
	}
	// WriteFile's mode only applies at creation and is subject to the
	// umask (umask 077, common under sudo, strips the group/other
	// bits); an explicit chmod guarantees the replaced binary is
	// executable for all users.
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, exePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func fetchBytes(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", checkUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// checksumFor parses sha256sum-style lines ("<hex>  <filename>") and
// returns the hex digest for name. Whitespace-tolerant, like
// install.sh's grep+awk.
func checksumFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0], true
		}
	}
	return "", false
}

// extractBinary pulls the top-level `ccwrap` entry out of the tar.gz —
// the exact archive shape install.sh consumes.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive did not contain ccwrap")
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if filepath.Base(hdr.Name) == "ccwrap" && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxArchiveSize))
		}
	}
}
