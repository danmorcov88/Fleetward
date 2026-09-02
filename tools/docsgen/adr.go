package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	adrTitleLine = regexp.MustCompile(`^# ADR-(\d{4}): (.+)$`)
	adrStatusRe  = regexp.MustCompile(`(?m)^- \*\*Status:\*\* (.+)$`)
	adrDateRe    = regexp.MustCompile(`(?m)^- \*\*Date:\*\* (\d{4}-\d{2}-\d{2})`)
)

type record struct {
	id, file, title, status, date string
}

// genADRIndex writes the decision-record index.
//
// Generated because the count was wrong the moment it was written by hand: the README claimed
// twenty ADRs in one paragraph and twenty-three in another, 346 lines apart. A number that has to
// be maintained is a number that will be wrong.
func genADRIndex(root string) (string, error) {
	dir := filepath.Join(root, "docs", "adr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading docs/adr: %w", err)
	}

	var records []record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || !isNumbered(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // enumerated from that directory
		if err != nil {
			return "", err
		}
		text := strings.ReplaceAll(string(b), "\r\n", "\n")

		first, _, _ := strings.Cut(text, "\n")
		m := adrTitleLine.FindStringSubmatch(first)
		if m == nil {
			return "", fmt.Errorf("%s: title is not `# ADR-NNNN: Title` (docscheck explains why)", name)
		}
		r := record{id: m[1], file: name, title: m[2]}
		if s := adrStatusRe.FindStringSubmatch(text); s != nil {
			r.status = strings.TrimSpace(s[1])
		}
		if d := adrDateRe.FindStringSubmatch(text); d != nil {
			r.date = d[1]
		}
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].id < records[j].id })

	var b strings.Builder
	b.WriteString(banner("docs/adr/*.md"))
	fmt.Fprintf(&b, `# Architecture decision records

Every architecturally significant decision, numbered and dated. An ADR is a record of what was
decided and when, so it is not rewritten when the world changes: a decision is changed by writing a
new ADR that supersedes it, and a detail that turned out differently is corrected by a dated note at
the top of the affected record ([ADR-0001](0001-record-architecture-decisions.md)).

Each carries Context, Decision, Consequences, and the alternatives that were rejected. `+
		"`make docs-check`"+` enforces that.

<!-- adr-count -->%d<!-- /adr-count --> records.

`, len(records))

	var rows [][]string
	for _, r := range records {
		rows = append(rows, []string{
			fmt.Sprintf("[%s](%s)", r.id, r.file),
			r.title,
			marker(r.status),
			r.date,
		})
	}
	b.WriteString(table([]string{"ADR", "Decision", "", "Date"}, rows))

	b.WriteString(`
## Reading the older records

**ADRs 0001–0014 were written on the same day**, from the original implementation brief, before any
code existed. They are sound, and two things about them are worth knowing:

- **They use "Stage N" for roadmap positions.** The roadmap has since been reorganised into phases
  and slices, and lives at [` + "`../roadmap.md`" + `](../roadmap.md). Where an early record says
  "Stage 3", read it as "a later slice"; the sequencing claim is usually still true and only the
  label is retired. The conformance suite's own **Stage 0** and **Stage 1** are a different and
  current concept — the two levels a plugin can pass — and are not affected.
- **A few describe intentions that implementation revised.** Those carry a dated correction note at
  the top, and are marked in the table above. The engineering
  [journal](../dev/journal/README.md) records when and why.

Records from 0015 onward were written alongside the work, and several name the slice that produced
them.
`)
	return b.String(), nil
}

func isNumbered(name string) bool {
	if len(name) < 4 {
		return false
	}
	for _, r := range name[:4] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// marker turns a status into a short column, so that a corrected or superseded record is visible
// from the index rather than only on opening it.
func marker(status string) string {
	lower := strings.ToLower(status)
	switch {
	case strings.Contains(lower, "superseded"):
		return "superseded in part"
	case strings.Contains(lower, "correction"):
		return "corrected"
	}
	return ""
}
