//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmorcov88/fleetward/tools/demo/acts"
)

// TestTheDemo is the end-to-end test, and it is the demo.
//
// One program, two entry points (ADR-0037). The obvious future simplification is to split them —
// to write a lean test that asserts the API and leave the demo to be a script somebody maintains by
// hand — and splitting them is how the demo starts lying. A demo nothing runs is a claim about the
// product with no gate behind it, and this repository's entire quality argument is that a claim is
// trustworthy because a merge gate enforces it.
//
// So there is nothing here but configuration and a call. Everything asserted is asserted inside the
// acts, in the same sentences the demo says out loud, which is what makes a failure in CI legible
// to somebody who has never seen the codebase: the error names the act, and the act names what it
// expected.
func TestTheDemo(t *testing.T) {
	cfg, err := acts.DefaultConfig()
	if err != nil {
		t.Fatalf("locate the repository: %v", err)
	}

	// The presentation off, the assertions on. The narration itself stays: a CI log of a failed
	// end-to-end run is far more useful when it reads as the story the run was telling when it
	// broke than when it reads as a bare comparison.
	cfg.Theatre = false
	cfg.Pause = 0
	cfg.Out = &testWriter{t: t}
	// CI checks out the tree and has no images, so the stack is built here rather than assumed.
	cfg.Build = true

	// Generous, and bounded. The run pulls a 625 MB image on a cold runner, starts eight
	// containers, takes two real backups and runs three real verifications, each of which stands up
	// a further container of its own.
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()

	if err := acts.Run(ctx, cfg); err != nil {
		t.Fatalf("%v", err)
	}
}

// testWriter routes the narration into the test log, one line at a time, so `go test -v` shows the
// acts as they happen and a failure carries everything that preceded it.
type testWriter struct {
	t   *testing.T
	buf strings.Builder
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.buf.Write(p)
	for {
		line := w.buf.String()
		i := strings.IndexByte(line, '\n')
		if i < 0 {
			break
		}
		w.t.Log(strings.TrimRight(line[:i], " \t\r"))
		w.buf.Reset()
		w.buf.WriteString(line[i+1:])
	}
	return len(p), nil
}
