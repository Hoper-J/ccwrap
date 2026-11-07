//go:build !windows

package certs

import "runtime"

func readPlatformSystemCAs() ([]byte, error) {
	if runtime.GOOS == "darwin" {
		return readDarwinSystemCAs()
	}
	return readLinuxSystemCAs()
}
