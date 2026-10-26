package supervisor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestModelsCellPresentation(t *testing.T) {
	cases := []struct {
		sess                 model.Session
		value, detail, state string
	}{
		{model.Session{}, "default", "", ""},
		{model.Session{ModelAliasCount: 1}, "1 alias", "", "aliases-active"},
		{model.Session{ModelAliasCount: 2, UnmappedModels: []string{"claude-opus-5-5"}}, "2 aliases", "1 unaliased model sent", "aliases-warn"},
		{model.Session{ModelAliasCount: 1, UnmappedModels: []string{"a", "b"}}, "1 alias", "2 unaliased models sent", "aliases-warn"},
	}
	for _, tc := range cases {
		v, d, st := modelsCellPresentation(tc.sess)
		if v != tc.value || d != tc.detail || st != tc.state {
			t.Errorf("%+v: got %q %q %q", tc.sess, v, d, st)
		}
	}
	// The first paint renders the same cell.
	sess := model.Session{ModelAliasCount: 1, UnmappedModels: []string{"claude-opus-5-5"}}
	for _, c := range webRibbonFromSession(sess, true, 1) {
		if c.Label == "Models" && (c.Value != "1 alias" || c.Detail != "1 unaliased model sent" || c.DataState != "aliases-warn") {
			t.Fatalf("first paint Models = %+v", c)
		}
	}
}

// The wire session carries the fix: the unaliased models, the profile, and
// the models this route's aliases use ("*=*" is not one of them).
func TestSessionDisplayCarriesModelFix(t *testing.T) {
	sess := model.Session{ActiveProfileName: "deepseek", ModelAliasCount: 2, UnmappedModels: []string{"claude-opus-5-5"},
		ModelAliasForward: map[string]string{"claude-opus-4-8": "deepseek-v4-flash", "claude-haiku-4-5": "deepseek-v4-flash"}}
	fix := sessionWithDisplay(sess).Display.ModelFix
	if fix == nil || fix.Profile != "deepseek" || !reflect.DeepEqual(fix.Models, []string{"claude-opus-5-5"}) || !reflect.DeepEqual(fix.Targets, []string{"deepseek-v4-flash"}) {
		t.Fatalf("model fix = %+v", fix)
	}
	if sessionWithDisplay(model.Session{ModelAliasCount: 1}).Display.ModelFix != nil {
		t.Fatal("nothing unaliased → no fix")
	}
}

// A live session update paints the Models cell with the server-rendered
// text, adding and removing the detail line.
func TestLiveModelsCellUsesServerText(t *testing.T) {
	js := liftScriptFns(t, renderedInlineScript(t), "updateModelsCell")
	paint := func(sess model.Session) string {
		data, err := json.Marshal(sessionWithDisplay(sess))
		if err != nil {
			t.Fatal(err)
		}
		return `state.session = ` + string(data) + `; updateModelsCell(); out.push([v.textContent, cell.querySelector('.d') ? cell.querySelector('.d').textContent : null, cell.dataset.state]);`
	}
	driver := domShimJS + `
var cell = document.createElement('div');
var v = document.createElement('div');
cell.appendChild(v);
cell.querySelector = function(sel){
  if (sel === '[data-ribbon-value]') return v;
  for (var i = 0; i < this.childNodes.length; i++) if (this.childNodes[i].className === 'd') return this.childNodes[i];
  return null;
};
function ribbonCellEl(){ return cell; }
var state = {}; var out = [];
` + js + `
` + paint(model.Session{ModelAliasCount: 1}) + `
` + paint(model.Session{ModelAliasCount: 1, UnmappedModels: []string{"claude-opus-5-5"}}) + `
` + paint(model.Session{}) + `
process.stdout.write(JSON.stringify(out));`
	out := runNode(t, driver)
	want := `[["1 alias",null,"aliases-active"],["1 alias","1 unaliased model sent","aliases-warn"],["default",null,""]]`
	if string(out) != want {
		t.Fatalf("live Models cell:\n got %s\nwant %s", out, want)
	}
}

// modelFixEl offers one button per target plus "keep Claude names" when the
// session has a profile, and only a relaunch hint without one.
func TestModelFixElement(t *testing.T) {
	js := liftScriptFns(t, renderedInlineScript(t), "modelFixEl", "modelFixButton")
	driver := domShimJS + "\n" + js + `
function walk(n){ return {cls: n.className, target: n.getAttribute('data-target'), text: n.children.length ? null : n.textContent, kids: n.children.map(walk)}; }
var withProfile = modelFixEl({models: ['claude-opus-5-5'], profile: 'deepseek', targets: ['deepseek-v4-flash']});
var noProfile = modelFixEl({models: ['claude-a', 'claude-b'], targets: ['x']});
process.stdout.write(JSON.stringify([walk(withProfile), walk(noProfile), modelFixEl(null) === null]));`
	out := runNode(t, driver)
	const want = `[{"cls":"model-fix","target":null,"text":null,"kids":[` +
		`{"cls":"mf-msg","target":null,"text":"Claude Code sent claude-opus-5-5 with no alias; the upstream got that name as-is.","kids":[]},` +
		`{"cls":"mf-actions","target":null,"text":null,"kids":[` +
		`{"cls":"mf-btn","target":"deepseek-v4-flash","text":"Send them to deepseek-v4-flash","kids":[]},` +
		`{"cls":"mf-btn","target":"*","text":"Keep Claude names","kids":[]}]},` +
		`{"cls":"mf-note","target":null,"text":"Saves the choice to profile deepseek and applies it now.","kids":[]}]},` +
		`{"cls":"model-fix","target":null,"text":null,"kids":[` +
		`{"cls":"mf-msg","target":null,"text":"Claude Code sent claude-a, claude-b with no alias; the upstream got those names as-is.","kids":[]},` +
		`{"cls":"mf-note","target":null,"text":"Relaunch with --model-alias '*=<model>', or '*=*' if the upstream serves Claude names.","kids":[]}]},true]`
	if string(out) != want {
		t.Fatalf("modelFixEl:\n got %s\nwant %s", out, want)
	}
}

// In the 4×2 ribbon (native TLS on, wider than 1024px) the Profile cell is in
// the first column, so its popover opens rightward; anchored to the cell's
// right edge it would run off the left of the window.
func TestProfilePopoverOpensRightwardIn4x2Ribbon(t *testing.T) {
	mustContainAllWeb(t, captureRenderedDashboardHTML(t),
		`@media (min-width:1025px){.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]) .sp3-pop{left:0;right:auto}}`)
}
