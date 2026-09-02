package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// adrTitle matches the one title form the corpus uses. ADRs 0021 to 0023 briefly used an em dash;
// that is exactly the kind of drift a single record's author never notices and a reader does.
var adrTitle = regexp.MustCompile(`^# ADR-(\d{4}): \S`)

// requiredSections are what CONTRIBUTING promises every record contains. "Alternatives considered"
// is the one that gets dropped when a decision felt obvious at the time — and it is the section a
// future reader needs most, because it is the only place the rejected option is written down.
var requiredSections = []string{
	"## Context", "## Decision", "## Consequences", "## Alternatives considered",
}

// knownStatuses is a closed set. An ADR whose status is freely worded cannot be indexed, and the
// index is how anyone finds the record that supersedes the one they are reading.
var knownStatuses = []string{"Accepted", "Accepted, with corrections", "Proposed", "Rejected"}

var (
	adrStatus     = regexp.MustCompile(`(?m)^- \*\*Status:\*\* (.+)$`)
	adrDate       = regexp.MustCompile(`(?m)^- \*\*Date:\*\* (\d{4}-\d{2}-\d{2})\s*$`)
	adrRefInText  = regexp.MustCompile(`ADR-(\d{4})`)
	adrIndexRow   = regexp.MustCompile(`^\| \[(\d{4})\]\(`)
	adrFilePrefix = regexp.MustCompile(`^docs/adr/(\d{4})-`)
)

// checkADRs enforces the record format, and the one relationship the corpus described in prose but
// never modelled: a supersession has two ends, and both must say so.
//
// ADR-0021 replaced ADR-0007's upload mechanism and said so; ADR-0007 said nothing, so a reader
// arriving at 0007 — the one an outdated link points to — had no way to learn it had been
// overtaken. That is the failure this check makes impossible.
func checkADRs(r *repo) []finding {
	var out []finding
	records := map[string]*doc{}

	for _, d := range r.docs {
		m := adrFilePrefix.FindStringSubmatch(d.path)
		if m == nil {
			continue
		}
		id := m[1]
		records[id] = d
		out = append(out, checkOneADR(d, id)...)
	}

	if len(records) == 0 {
		return out
	}
	out = append(out, checkSupersessions(records)...)
	out = append(out, checkADRIndex(r, records)...)
	return out
}

func checkOneADR(d *doc, id string) []finding {
	var out []finding
	text := d.text()

	if m := adrTitle.FindStringSubmatch(d.lines[0]); m == nil {
		out = append(out, finding{
			file: d.path, line: 1,
			msg:  "title is not `# ADR-NNNN: Title`",
			hint: "the corpus uses a colon; an em dash reads the same and sorts differently",
		})
	} else if m[1] != id {
		out = append(out, finding{
			file: d.path, line: 1,
			msg: fmt.Sprintf("title says ADR-%s but the filename says %s", m[1], id),
		})
	}

	if m := adrStatus.FindStringSubmatch(text); m == nil {
		out = append(out, finding{file: d.path, line: 0, msg: "has no `- **Status:**` line"})
	} else if status := strings.TrimSpace(m[1]); !isKnownStatus(status) {
		out = append(out, finding{
			file: d.path, line: 0,
			msg: fmt.Sprintf("status %q is not one of the known statuses", status),
			hint: "known: " + strings.Join(knownStatuses, ", ") +
				" — or a `superseded by` phrase naming the record that replaced it",
		})
	}

	if !adrDate.MatchString(text) {
		out = append(out, finding{
			file: d.path, line: 0,
			msg: "has no `- **Date:** YYYY-MM-DD` line",
		})
	}

	for _, section := range requiredSections {
		if !containsHeading(d.lines, section) {
			out = append(out, finding{
				file: d.path, line: 0,
				msg: fmt.Sprintf("has no `%s` section", section),
				hint: "the alternatives are the part a future reader needs most — they are the " +
					"only place a rejected option is written down",
			})
		}
	}
	return out
}

func isKnownStatus(s string) bool {
	for _, k := range knownStatuses {
		if s == k {
			return true
		}
	}
	// A status may also spell out a supersession rather than being a bare word.
	return strings.Contains(strings.ToLower(s), "superseded")
}

func containsHeading(lines []string, heading string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) == heading {
			return true
		}
	}
	return false
}

// checkSupersessions requires both ends of a supersession to agree.
func checkSupersessions(records map[string]*doc) []finding {
	var out []finding

	// claims[a][b] means "a says it supersedes b".
	claims := map[string]map[string]bool{}
	// noted[b][a] means "b says it is superseded by a".
	noted := map[string]map[string]bool{}

	for id, d := range records {
		for _, line := range d.lines {
			lower := strings.ToLower(line)
			targets := adrRefInText.FindAllStringSubmatch(line, -1)
			switch {
			case strings.Contains(lower, "supersedes"):
				for _, t := range targets {
					add(&claims, id, t[1])
				}
			case strings.Contains(lower, "superseded by"):
				for _, t := range targets {
					add(&noted, id, t[1])
				}
			}
		}
	}

	for a, targets := range claims {
		for b := range targets {
			if b == a {
				continue
			}
			if _, ok := records[b]; !ok {
				out = append(out, finding{
					file: records[a].path, line: 0,
					msg: fmt.Sprintf("says it supersedes ADR-%s, which does not exist", b),
				})
				continue
			}
			if !noted[b][a] {
				out = append(out, finding{
					file: records[b].path, line: 0,
					msg: fmt.Sprintf("is superseded by ADR-%s but does not say so", a),
					hint: "a reader arriving here from an old link has no other way to learn it — " +
						"add a `superseded by` note near the status",
				})
			}
		}
	}

	for b, sources := range noted {
		for a := range sources {
			if a == b {
				continue
			}
			if _, ok := records[a]; !ok {
				out = append(out, finding{
					file: records[b].path, line: 0,
					msg: fmt.Sprintf("says it is superseded by ADR-%s, which does not exist", a),
				})
				continue
			}
			if !claims[a][b] {
				out = append(out, finding{
					file: records[a].path, line: 0,
					msg: fmt.Sprintf("ADR-%s says this record supersedes it, but this record does "+
						"not say so", b),
				})
			}
		}
	}
	return out
}

func add(m *map[string]map[string]bool, from, to string) {
	if (*m)[from] == nil {
		(*m)[from] = map[string]bool{}
	}
	(*m)[from][to] = true
}

// checkADRIndex asserts docs/adr/README.md lists every record, and that any prose count of the
// records agrees with how many there are. A hand-maintained count is wrong within two decisions:
// the README claimed twenty in one paragraph and twenty-three in another, 346 lines apart.
func checkADRIndex(r *repo, records map[string]*doc) []finding {
	var out []finding

	var index *doc
	for _, d := range r.docs {
		if d.path == "docs/adr/README.md" {
			index = d
			break
		}
	}
	if index == nil {
		return []finding{{
			file: "docs/adr/README.md", line: 0,
			msg: "there is no ADR index",
		}}
	}

	listed := map[string]bool{}
	for _, l := range index.lines {
		if m := adrIndexRow.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			listed[m[1]] = true
		}
	}

	var missing []string
	for id := range records {
		if !listed[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		out = append(out, finding{
			file: index.path, line: 0,
			msg:  fmt.Sprintf("does not list ADR-%s", id),
			hint: "an unindexed record is one nobody will find",
		})
	}

	out = append(out, checkCountMarker(r, "adr-count", len(records))...)
	return out
}

// countMarker matches `<!-- name -->value<!-- /name -->`, which is how a number that must stay true
// is written: the marker is what lets a check find it, and later what lets a generator replace it.
func countMarker(name string) *regexp.Regexp {
	return regexp.MustCompile(`<!-- ` + name + ` -->(.*?)<!-- /` + name + ` -->`)
}

func checkCountMarker(r *repo, name string, want int) []finding {
	var out []finding
	re := countMarker(name)
	digits := regexp.MustCompile(`\d+`)

	for _, d := range r.docs {
		for i, line := range d.lines {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			got := digits.FindString(m[1])
			if got == "" {
				continue // a marker may wrap a word rather than a number
			}
			if got != fmt.Sprint(want) {
				out = append(out, finding{
					file: d.path, line: i + 1,
					msg: fmt.Sprintf("claims %s is %s; it is %d", name, got, want),
				})
			}
		}
	}
	return out
}
