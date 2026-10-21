//go:build linux

package localpeer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const foreignDialEnv = "LOCALPEER_TEST_DIAL"

// TestForeignDialHelper is the child half of TestLiveForeignUIDRefused: it
// only runs when re-executed with LOCALPEER_TEST_DIAL set, dials that address
// and prints what came back.
func TestForeignDialHelper(t *testing.T) {
	addr := os.Getenv(foreignDialEnv)
	if addr == "" {
		t.Skip("helper process only")
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("RESULT dial-error %v\n", err)
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		fmt.Printf("RESULT closed %v\n", err)
		return
	}
	fmt.Printf("RESULT served %s\n", strings.TrimSpace(line))
}

// A peer that wrote and closed before the lookup ran is listed orphaned
// (FIN_WAIT2 / TIME_WAIT, inode 0) with uid 0. It must be refused, not read
// as root — otherwise any user could fire one request and hang up.
func TestLiveOrphanedPeerRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(gone, "GET /recent HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	_ = gone.Close()
	time.Sleep(200 * time.Millisecond) // let the closed end settle into FIN_WAIT2

	var mu sync.Mutex
	var logs []string
	wrapped := NewListener(ln, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	defer wrapped.Close()
	live, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	c, err := wrapped.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.RemoteAddr().String() != live.LocalAddr().String() {
		t.Fatalf("admitted %v, want only the live dialer %v (the orphaned one is %v)", c.RemoteAddr(), live.LocalAddr(), gone.LocalAddr())
	}
	// The two lookups run concurrently; give the orphan's a moment to log.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		joined := strings.Join(logs, "\n")
		mu.Unlock()
		if strings.Contains(joined, ErrOrphaned.Error()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan refusal not logged: %q", joined)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Root-only: a connection from another uid (nobody) is refused end to end
// while this process's own connection is served.
func TestLiveForeignUIDRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a child as another uid")
	}
	const nobody = 65534
	// The test binary normally sits in a 0700 build dir; give the child a
	// copy it can execute.
	dir, err := os.MkdirTemp("", "localpeer-foreign-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "localpeer.test")
	src, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	wrapped := NewListener(ln, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	defer wrapped.Close()
	go func() {
		for {
			c, err := wrapped.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "ok\n")
			_ = c.Close()
		}
	}()

	cmd := exec.Command(bin, "-test.run=^TestForeignDialHelper$", "-test.count=1")
	cmd.Dir = "/"
	cmd.Env = []string{foreignDialEnv + "=" + ln.Addr().String()}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper as uid %d: %v\n%s", nobody, err, out)
	}
	if !strings.Contains(string(out), "RESULT closed") {
		t.Fatalf("uid %d connection was not refused:\n%s", nobody, out)
	}

	own, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	_ = own.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(own).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ok" {
		t.Fatalf("own connection not served: %q, %v", line, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, fmt.Sprintf("refused connection from uid %d", nobody)) {
		t.Fatalf("refusal not logged: %q", joined)
	}
	t.Logf("child output: %s", strings.TrimSpace(string(out)))
}
