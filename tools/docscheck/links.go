package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var mdLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// checkLinks asserts that every relative markdown link resolves.
//
// The cost of a dead link is small individually and large in aggregate: documentation nobody can
// navigate stops being read, and this project has just moved a great many files. It is also the
// check that makes future restructuring safe, which is the point — the split of STATUS.md into a
// journal moved four hundred lines, and doing that confidently requires a net underneath.
func checkLinks(r *repo) []finding {
	var out []finding
	for _, d := range r.docs {
		kinds := classify(d.lines)
		for i, line := range d.lines {
			if kinds[i] != proseLine {
				continue // a link inside a fenced block is an example, not a link
			}
			for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
				target := strings.TrimSpace(m[1])
				if skipLink(target) {
					continue
				}
				// Strip an anchor: the file has to exist, but heading slugs are GitHub's business.
				path, _, _ := strings.Cut(target, "#")
				if path == "" {
					continue // a bare "#anchor" is a link within the page
				}
				resolved := filepath.ToSlash(filepath.Join(filepath.Dir(d.path), path))
				if !r.exists(resolved) {
					out = append(out, finding{
						file: d.path, line: i + 1,
						msg: fmt.Sprintf("link to %q resolves to %q, which does not exist",
							target, resolved),
					})
				}
			}
		}
	}
	return out
}

// skipLink reports whether a link target is not ours to resolve.
func skipLink(t string) bool {
	switch {
	case t == "":
		return true
	case strings.HasPrefix(t, "#"):
		return true
	case strings.Contains(t, "://"):
		return true
	case strings.HasPrefix(t, "mailto:"):
		return true
	case strings.HasPrefix(t, "<"):
		return true // an autolink, already handled by the cases above once unwrapped
	}
	return false
}
