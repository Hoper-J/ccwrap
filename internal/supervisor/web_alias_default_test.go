package supervisor

import "testing"

// The "other Claude models" select: add mode follows the one model every
// alias uses until the user picks; edit mode keeps the stored choice and
// only marks the suggestion, so saving an unrelated edit adds nothing.
func TestAliasDefaultChoices(t *testing.T) {
	js := liftScriptFns(t, renderedInlineScript(t), "aliasDefaultChoices")
	driver := js + `
var cases = [
  aliasDefaultChoices(['deepseek-v4-flash'], '', 'add', false),
  aliasDefaultChoices(['deepseek-v4-flash'], '', 'add', true),
  aliasDefaultChoices(['deepseek-v4-flash'], '', 'edit', false),
  aliasDefaultChoices(['big', 'small'], '', 'edit', false),
  aliasDefaultChoices(['gpt-6'], '*', 'edit', false),
  aliasDefaultChoices(['gpt-6'], 'gpt-7', 'edit', false),
];
process.stdout.write(JSON.stringify(cases));`
	out := runNode(t, driver)
	const want = `[` +
		`{"options":[["","not set"],["deepseek-v4-flash","deepseek-v4-flash"],["*","keep their Claude names"],["__custom","other model…"]],"value":"deepseek-v4-flash"},` +
		`{"options":[["","not set"],["deepseek-v4-flash","deepseek-v4-flash"],["*","keep their Claude names"],["__custom","other model…"]],"value":""},` +
		`{"options":[["","not set"],["deepseek-v4-flash","deepseek-v4-flash (suggested)"],["*","keep their Claude names"],["__custom","other model…"]],"value":""},` +
		`{"options":[["","not set"],["big","big"],["small","small"],["*","keep their Claude names"],["__custom","other model…"]],"value":""},` +
		`{"options":[["","not set"],["gpt-6","gpt-6"],["*","keep their Claude names"],["__custom","other model…"]],"value":"*"},` +
		`{"options":[["","not set"],["gpt-6","gpt-6"],["gpt-7","gpt-7"],["*","keep their Claude names"],["__custom","other model…"]],"value":"gpt-7"}]`
	if string(out) != want {
		t.Fatalf("aliasDefaultChoices:\n got %s\nwant %s", out, want)
	}
}
