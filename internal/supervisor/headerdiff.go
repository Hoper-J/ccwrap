package supervisor

import (
	"net/http"
	"slices"
	"sort"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// upstreamHeaderChanges compares a request's headers as Claude Code sent
// them (in) with the ones going upstream (out): a header only in out was
// injected, one only in in was stripped, one whose values differ was
// rewritten. The result names headers only, never values.
func upstreamHeaderChanges(in, out http.Header) *model.HeaderDiff {
	d := &model.HeaderDiff{Changes: []model.HeaderChange{}}
	for name, vs := range out {
		was, ok := in[name]
		switch {
		case !ok:
			d.Changes = append(d.Changes, model.HeaderChange{Name: name, Action: model.HeaderInjected})
		case !slices.Equal(was, vs):
			d.Changes = append(d.Changes, model.HeaderChange{Name: name, Action: model.HeaderRewritten})
		}
	}
	for name := range in {
		if _, ok := out[name]; !ok {
			d.Changes = append(d.Changes, model.HeaderChange{Name: name, Action: model.HeaderStripped})
		}
	}
	sort.Slice(d.Changes, func(i, j int) bool { return d.Changes[i].Name < d.Changes[j].Name })
	return d
}
