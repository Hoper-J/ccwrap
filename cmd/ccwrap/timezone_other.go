//go:build !windows

package main

import (
	"os"
	"strings"
)

// systemZoneName reads /etc/localtime and, if it is a symlink into a
// zoneinfo tree ("/usr/share/zoneinfo/Asia/Shanghai"), returns the IANA suffix
// ("Asia/Shanghai"). Returns "" when /etc/localtime is not a zoneinfo symlink.
func systemZoneName() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	const marker = "/zoneinfo/"
	if idx := strings.LastIndex(target, marker); idx >= 0 {
		return target[idx+len(marker):]
	}
	return ""
}
