package acts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// CastWidth and CastHeight are the terminal the recording claims to have been made in.
//
// The narrator wraps prose at 78 and caps a table at CastWidth, so nothing the demo prints needs
// more room than this. A recording wider than its content is letterboxed; one narrower reflows, and
// a reflowed table is unreadable at video sizes.
const (
	CastWidth = 100
	// Taller than a stock terminal on purpose. The demo emits up to forty lines in one burst — an
	// act banner, a table and the sentences around it, all written with no delay between them — and
	// a shorter screen scrolls the top of that away before a renderer captures a frame.
	CastHeight = 46
)

// castWriter tees the narration into an asciicast v2 file.
//
// The demo does not need a terminal recorder, and on Windows there is not a usable one: asciinema's
// CLI is Unix-only. What a recorder produces is a JSON header and one `[seconds, "o", text]` line
// per write, and the demo already knows both halves — it is the thing doing the writing, and its
// narrator already measures elapsed time. So it writes its own, and `agg` turns that into a GIF.
//
// The recording is therefore of a real run: every timestamp is when that line was actually printed,
// and nothing is re-enacted afterwards.
type castWriter struct {
	mu    sync.Mutex
	file  *os.File
	start time.Time
	err   error
}

// newCastWriter opens path and writes the asciicast header.
//
// idleLimit is carried in the header so a player and `agg` both cap the long waits — a verification
// that takes fifteen seconds is honest and is dead air in a recording. The gaps are shortened on
// playback, never removed from the file, so the real timings stay in it.
func newCastWriter(path string, idleLimit time.Duration) (*castWriter, error) {
	file, err := os.Create(path) //nolint:gosec // G304: operator-supplied output path
	if err != nil {
		return nil, fmt.Errorf("create the recording: %w", err)
	}

	header := map[string]any{
		"version":         2,
		"width":           CastWidth,
		"height":          CastHeight,
		"timestamp":       time.Now().Unix(),
		"title":           "Fleetward — make demo",
		"idle_time_limit": idleLimit.Seconds(),
		"env":             map[string]string{"TERM": "xterm-256color", "SHELL": "/bin/bash"},
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("encode the recording header: %w", err)
	}
	if _, err := fmt.Fprintf(file, "%s\n", encoded); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write the recording header: %w", err)
	}
	return &castWriter{file: file, start: time.Now()}, nil
}

// Write records one output event.
//
// The newline translation is not cosmetic. A recording is replayed into a terminal emulator, where
// "\n" moves the cursor down a line and leaves it in the column it was already in — so a file
// written with bare newlines plays back as a staircase running off the right-hand edge. A real
// recorder captures what the terminal driver actually sent, which is "\r\n".
func (w *castWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return len(p), nil
	}

	event := []any{
		time.Since(w.start).Seconds(),
		"o",
		strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n", "\r\n"),
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		w.err = err
		return len(p), nil
	}
	if _, err := fmt.Fprintf(w.file, "%s\n", encoded); err != nil {
		w.err = err
	}
	// Never fails the demo. A recording is an artifact of the run, not part of it, and losing the
	// artifact is not a reason to lose the run.
	return len(p), nil
}

// Close finishes the recording and reports anything that went wrong along the way.
func (w *castWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Close(); err != nil && w.err == nil {
		w.err = err
	}
	return w.err
}

// NewCastRecorder returns a writer that records everything written to it, and a closer.
//
// Exported because `main` owns the decision to record, the same way it owns `-transcript`.
func NewCastRecorder(path string, idleLimit time.Duration) (interface{ Write([]byte) (int, error) }, func() error, error) {
	w, err := newCastWriter(path, idleLimit)
	if err != nil {
		return nil, nil, err
	}
	return w, w.Close, nil
}
