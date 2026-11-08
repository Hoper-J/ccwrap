package main

import "testing"

// Windows names the local zone by its ID; without it only the +8h offset
// guess is left, which also takes Singapore, Taipei and Perth for China.
func TestSystemZoneNameReadsTheWindowsID(t *testing.T) {
	if systemZoneName() == "" {
		t.Fatal("no time zone ID from the registry")
	}
}
