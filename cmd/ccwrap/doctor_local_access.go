package main

import (
	"fmt"
	"os"

	"github.com/Hoper-J/ccwrap/internal/localpeer"
)

// localAccessExposure is what unchecked session and ccwrap ui ports mean in
// practice. It goes in the summary because doctor shows details only with
// --verbose.
const localAccessExposure = "other local users could use a session's proxy and read ccwrap ui"

// addLocalAccessCheck reports whether session ports and ccwrap ui will refuse
// other local users: it runs the same peer-uid lookup their listeners use
// against a throwaway loopback connection from this process.
func addLocalAccessCheck(add func(name, status, summary, detail string), getenv func(string) string, selfTest func() (uint32, error)) {
	if localpeer.Disabled(getenv) {
		add("local_access", "warn", fmt.Sprintf("session port access check disabled (%s=0); %s", localpeer.DisableEnv, localAccessExposure), "")
		return
	}
	self := uint32(os.Geteuid())
	uid, err := selfTest()
	switch {
	case err != nil:
		add("local_access", "warn", fmt.Sprintf("session port access check unavailable (%v); %s", err, localAccessExposure), "")
	case uid != self:
		add("local_access", "fail", fmt.Sprintf("session port access check misidentified this process as uid %d (expected %d); Claude Code's own connections could be refused", uid, self),
			fmt.Sprintf("set %s=0 to turn the check off", localpeer.DisableEnv))
	default:
		add("local_access", "pass", fmt.Sprintf("session ports and ccwrap ui accept only your own processes (uid %d) and root", self), "")
	}
}
