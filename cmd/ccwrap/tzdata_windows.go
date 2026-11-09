package main

// Windows has no IANA time zone database outside a Go install, so
// time.LoadLocation would reject every zone --timezone or CCWRAP_TZ names;
// embed one.
import _ "time/tzdata"
