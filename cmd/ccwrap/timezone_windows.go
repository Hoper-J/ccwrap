package main

import "golang.org/x/sys/windows/registry"

// systemZoneName is Windows's ID for the local time zone, such as "China
// Standard Time", or "" when it can't be read: Windows has no /etc/localtime.
func systemZoneName() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	id, _, err := k.GetStringValue("TimeZoneKeyName")
	if err != nil {
		return ""
	}
	return id
}
