//go:build !windows

package localpeer

import "fmt"

// RootName is what messages call the superuser, whose connections pass.
const RootName = "root"

func ownerName(uid, _ uint32) string { return fmt.Sprintf("uid %d", uid) }
