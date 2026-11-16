package cli

import (
	"io"
	"strings"
)

// park parks stdout while the narration runs, for the formats whose verdict is one block. Under
// json and sarif the narration prints to nowhere: a build log is never replayed into an object. Under ai it
// is buffered: every result's block replaces it, a failed run's too, and a run with no result to
// render prints it whole (cli.md §5.2). A verb calls it once after parse, so --help prints in every format, and
// defers unpark(true) so every return hands stdout back; a second call is a no-op.
func (a *app) park() {
	if a.live != nil {
		return
	}
	switch a.render.Format {
	case FormatJSON, FormatSARIF:
		a.live, a.out = a.out, io.Discard
	case FormatAI:
		a.live, a.parked = a.out, &strings.Builder{}
		a.out = a.parked
	}
}

// unpark hands stdout back: the buffered transcript prints when keep is true (a run with no result
// to render, and so no block) and is dropped when a block replaces it: the ai verdict, or any refusal from
// fail. Under json there is no buffer.
func (a *app) unpark(keep bool) {
	if a.live == nil {
		return
	}
	a.out = a.live
	if keep && a.parked != nil {
		_, _ = io.WriteString(a.out, a.parked.String())
	}
	a.live, a.parked = nil, nil
}
