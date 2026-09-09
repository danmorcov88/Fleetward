package acts

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Narrator is how the acts speak.
//
// Two modes, one code path. With Theatre on it is a presentation: an act banner, a pause between
// acts so a viewer can read the screen, and the sentences that exist for a person watching. With it
// off — which is how CI runs — the same lines are written without ceremony and without pauses, so a
// failed end-to-end run in a CI log reads as the story it was telling when it broke.
//
// Nothing here uses colour or cursor control. A transcript is a file somebody reads later, and the
// terminal recording is made by asciinema around the whole program rather than by escape codes
// inside it.
type Narrator struct {
	out     io.Writer
	theatre bool
	pause   time.Duration
	started time.Time
}

// NewNarrator builds a narrator over cfg's writer.
func NewNarrator(cfg Config) *Narrator {
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	return &Narrator{out: out, theatre: cfg.Theatre, pause: cfg.Pause, started: time.Now()}
}

// Act announces one act. The `why` line is the point of the act in one sentence, and it is there
// because a demo that shows what happens without saying why it matters is a screencast.
func (n *Narrator) Act(number int, title, why string) {
	n.blank()
	rule := strings.Repeat("─", 78)
	if n.theatre {
		fmt.Fprintf(n.out, "%s\n", rule)
	}
	fmt.Fprintf(n.out, "ACT %d — %s\n", number, title)
	if why != "" {
		fmt.Fprintf(n.out, "%s\n", wrap(why, 78))
	}
	if n.theatre {
		fmt.Fprintf(n.out, "%s\n", rule)
	}
	n.blank()
}

// Say writes a sentence addressed to the person watching.
func (n *Narrator) Say(format string, args ...any) {
	fmt.Fprintf(n.out, "%s\n", wrap(fmt.Sprintf(format, args...), 78))
}

// Step writes one line of progress. A real restore takes tens of seconds, and a demo that looks
// hung is a demo somebody stops watching.
func (n *Narrator) Step(format string, args ...any) {
	n.hanging("  "+n.elapsed()+"  ", fmt.Sprintf(format, args...))
}

// Seeded labels a fact as fixture rather than as something the product just did.
//
// It is a method rather than a convention so that it cannot be forgotten: every sentence about
// history that Fleetward did not observe goes through here and comes out marked.
func (n *Narrator) Seeded(format string, args ...any) {
	n.hanging("  [seeded]  ", fmt.Sprintf(format, args...))
}

// hanging writes prefix once and wraps the rest under it, so a long line stays inside the terminal
// the recording claims to be in rather than reflowing into a staircase.
func (n *Narrator) hanging(prefix, text string) {
	body := wrap(text, CastWidth-len(prefix))
	fmt.Fprintf(n.out, "%s%s\n", prefix,
		strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", len(prefix))))
}

// Table renders a fixed-width table. Used for the estate answers, which are the acts whose whole
// content is a table.
func (n *Narrator) Table(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// The last column is the one that carries a sentence — a retention reason, what an instance is
	// here to show — and left to itself it makes a table two hundred columns wide, which is a table
	// nobody can read and a recording nobody can watch. It gets whatever room is left and wraps
	// inside itself, aligned under its own heading.
	indent := 2
	fixed := indent
	for _, w := range widths[:len(widths)-1] {
		fixed += w + 2
	}
	last := CastWidth - fixed
	if last < 20 {
		last = 20
	}
	if widths[len(widths)-1] > last {
		widths[len(widths)-1] = last
	}

	render := func(cells []string) {
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", indent))
		for i, cell := range cells {
			if i >= len(widths) {
				break
			}
			if i == len(cells)-1 {
				// Continuation lines are indented to the column the cell starts in, so a wrapped
				// sentence still reads as belonging to its row.
				b.WriteString(strings.ReplaceAll(wrap(cell, widths[i]), "\n",
					"\n"+strings.Repeat(" ", fixed)))
				break
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
		}
		fmt.Fprintln(n.out, strings.TrimRight(b.String(), " "))
	}

	render(headers)
	dashes := make([]string, len(headers))
	for i := range headers {
		dashes[i] = strings.Repeat("-", widths[i])
	}
	render(dashes)
	for _, row := range rows {
		render(row)
	}
}

// Beat pauses so a viewer can read what is on the screen. It does nothing in CI, which is the
// difference between a demo and a test.
func (n *Narrator) Beat() {
	if n.theatre && n.pause > 0 {
		time.Sleep(n.pause)
	}
}

func (n *Narrator) blank() { fmt.Fprintln(n.out) }

func (n *Narrator) elapsed() string {
	d := time.Since(n.started).Round(time.Second)
	return fmt.Sprintf("%3d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// wrap breaks a sentence at width, so a narration line reads the same in a terminal as in a
// transcript file and in a recording.
//
// A word longer than the whole width is broken rather than left to overflow. That case is not
// hypothetical: an artifact's object key is a hundred and thirty characters of tenant, instance and
// backup identifiers with no spaces in it, and it is worth showing in full.
func wrap(s string, width int) string {
	if width < 8 {
		width = 8
	}
	var words []string
	for _, w := range strings.Fields(s) {
		for len(w) > width {
			words = append(words, w[:width])
			w = w[width:]
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return ""
	}

	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line)
			b.WriteByte('\n')
			line = w
			continue
		}
		line += " " + w
	}
	b.WriteString(line)
	return b.String()
}
