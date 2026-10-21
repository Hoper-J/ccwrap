package supervisor

import (
	"log"
	"net"
	"os"

	"github.com/Hoper-J/ccwrap/internal/localpeer"
)

// newLocalPeerListener restricts a session port to processes of this user and
// root. The port carries both the credential-injecting proxy and the
// dashboard, and loopback binding alone admits every account on the machine.
// CCWRAP_LOCAL_PEER_CHECK=0 (read at session-proxy creation) leaves ln as is.
// Refusals go to the supervisor log, never stderr (Claude Code's terminal).
func newLocalPeerListener(ln net.Listener, logger *log.Logger) net.Listener {
	if localpeer.Disabled(os.Getenv) {
		logger.Printf("local peer check disabled (%s=0): session port %s accepts connections from any local user", localpeer.DisableEnv, ln.Addr())
		return ln
	}
	return localpeer.NewListener(ln, logger.Printf)
}
