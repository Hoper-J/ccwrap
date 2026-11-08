package main

import (
	"strings"
	"testing"
)

func TestOpenInBrowserChoosesOpener(t *testing.T) {
	var ran []string
	run := func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	link := "http://127.0.0.1:5/"
	cases := []struct {
		goos   string
		env    map[string]string
		opened bool
		ran    string
	}{
		{"darwin", nil, true, "open " + link},
		{"darwin", map[string]string{"SSH_CONNECTION": "1 2 3 4"}, false, ""},
		{"linux", nil, false, ""},
		{"linux", map[string]string{"DISPLAY": ":0"}, true, "xdg-open " + link},
		{"windows", nil, true, "rundll32 url.dll,FileProtocolHandler " + link},
		{"windows", map[string]string{"SSH_CONNECTION": "1 2 3 4"}, false, ""},
	}
	for _, tc := range cases {
		ran = nil
		opened, err := openInBrowser(link, env(tc.env), tc.goos, run)
		if err != nil || opened != tc.opened || strings.Join(ran, "") != tc.ran {
			t.Errorf("%s %v: opened=%v ran=%q err=%v", tc.goos, tc.env, opened, ran, err)
		}
	}
}
