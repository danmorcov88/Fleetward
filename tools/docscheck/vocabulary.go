package main

import (
	"fmt"
	"regexp"
	"strings"
)

// banned are phrases that were true once and are not any more. A retired word is worse than a wrong
// one: it reads as meaningful, so a reader spends effort looking for what it refers to.
type bannedPhrase struct {
	pattern *regexp.Regexp
	why     string
	// exempt reports whether this occurrence is legitimate. It sees the path and the line, because
	// some exemptions are about the document and some are about the sentence.
	exempt func(path, line string) bool
}

var banned = []bannedPhrase{
	{
		// Deliberately [2-9], not [1-9]. The conformance suite's own Stage 0 and Stage 1 are a
		// current concept — the two levels a plugin can pass — and they are written about
		// constantly. Only the roadmap numbers above them are retired, so this catches "Stage 3"
		// and "Stage 6" without a false positive on every page that explains conformance.
		pattern: regexp.MustCompile(`\bStage [2-9]\b`),
		why:     `the roadmap uses phases and slices; "Stage N" above 1 is retired`,
		exempt:  func(path, _ string) bool { return isHistorical(path) },
	},
	{
		pattern: regexp.MustCompile(`\bPhase F\b`),
		why:     "there is no Phase F — production readiness is a property of every slice (ADR-0024)",
		exempt: func(path, line string) bool {
			// A sentence that says the thing does not exist has to be able to name it, and so does
			// the record that retired it.
			return isHistorical(path) ||
				strings.Contains(path, "0024-production-readiness") ||
				strings.Contains(line, "no Phase F")
		},
	},
	{
		pattern: regexp.MustCompile(`(?i)\bnine[- ]engine\b|\b9[- ]engine\b`),
		why: "the engine list is eight and lives in docs/engines.md; " +
			"a number that needs defending is worse than a list",
		exempt: func(path, _ string) bool { return isHistorical(path) },
	},
	{
		pattern: regexp.MustCompile(`Phase 1 acceptance criteria`),
		why:     `CLAUDE.md §7 is "Acceptance criteria for the first release"; "Phase 1" maps to no phase`,
		exempt:  func(path, _ string) bool { return isHistorical(path) },
	},
}

// romanian matches function words that appear in Romanian and not in English. Crude, and effective:
// it is the presence of any of them, not their grammar, that says a paragraph slipped through in
// the wrong language. CLAUDE.md's own rule is that all code, comments, and docs are in English, and
// a session-start prompt sat in Romanian in the protocol document for six weeks.
//
// Words that exist in both languages are deliberately absent. "care" is a Romanian relative pronoun
// and an ordinary English noun, and including it flagged "build with extra care" — a checker that
// cries wolf is one people learn to run with a filter.
var romanian = regexp.MustCompile(`(?i)\b(pentru|este|sunt|să|și|dacă|acum|trebuie|fără|orice|astfel)\b`)

// romanianExempt covers the one place the language rule is deliberately broken: a section title
// that is a Romanian phrase on purpose, quoted as a value rather than used as prose.
var romanianExempt = regexp.MustCompile(`Grandios|dar disciplinat`)

// isHistorical reports whether a document records what was true when it was written and is not
// rewritten afterwards. The journal is append-only, the Phase A briefs were written while the old
// vocabulary was current, ADRs 0001–0014 are immutable records from the original brief, and the ADR
// index is the page that explains all of this. Rewriting any of them would destroy a record to tidy
// a label.
func isHistorical(path string) bool {
	switch {
	case strings.HasPrefix(path, "docs/dev/journal/"):
		return true
	case strings.HasPrefix(path, "docs/dev/slices/A"):
		return true
	case path == "docs/adr/README.md":
		return true
	case earlyADR.MatchString(path):
		return true
	}
	return false
}

// earlyADR matches ADRs 0001 to 0014, all written on one day from the original implementation
// brief, before any code existed. Their vocabulary is of that moment.
var earlyADR = regexp.MustCompile(`^docs/adr/00(0[1-9]|1[0-4])-`)

// isMention reports whether the matched phrase is being named rather than used — the use/mention
// distinction, and the reason this check does not need a growing list of per-file exemptions.
//
// A document that retires a label has to be able to say which label it retired, and the way English
// does that is with quotation marks: `Those labels are retired` two lines below `"Phase F"`. A
// backticked or quoted occurrence is talking about the word; a bare one is using it.
func isMention(line, match string) bool {
	i := strings.Index(line, match)
	if i <= 0 || i+len(match) >= len(line) {
		return false
	}
	before, after := line[i-1], line[i+len(match)]
	for _, pair := range []struct{ open, close byte }{{'"', '"'}, {'`', '`'}, {'\'', '\''}} {
		if before == pair.open && after == pair.close {
			return true
		}
	}
	// Curly quotes are multi-byte, so look at the runes around the match instead.
	return strings.Contains(line, "“"+match+"”")
}

// checkVocabulary reports retired vocabulary and non-English prose.
func checkVocabulary(r *repo) []finding {
	var out []finding
	for _, d := range r.docs {
		kinds := classify(d.lines)
		for i, line := range d.lines {
			if kinds[i] != proseLine {
				continue
			}
			for _, b := range banned {
				if b.exempt != nil && b.exempt(d.path, line) {
					continue
				}
				m := b.pattern.FindString(line)
				if m == "" || isMention(line, m) {
					continue
				}
				out = append(out, finding{
					file: d.path, line: i + 1,
					msg:  fmt.Sprintf("uses retired vocabulary %q", m),
					hint: b.why,
				})
			}
			if romanianExempt.MatchString(line) {
				continue
			}
			if m := romanian.FindString(line); m != "" {
				out = append(out, finding{
					file: d.path, line: i + 1,
					msg:  fmt.Sprintf("looks like Romanian prose (%q)", m),
					hint: "CLAUDE.md: all code, comments, and docs in English",
				})
			}
		}
	}
	return out
}
