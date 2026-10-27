package main

import (
	"strings"
	"testing"
)

func TestSetEnvVarReplaces(t *testing.T) {
	got := setEnvVar([]string{"A=1", sessionIDEnv + "=outer", "B=2"}, sessionIDEnv, "inner")
	if strings.Join(got, ",") != "A=1,B=2,"+sessionIDEnv+"=inner" {
		t.Fatalf("env = %v", got)
	}
}
