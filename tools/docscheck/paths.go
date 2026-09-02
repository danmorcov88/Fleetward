package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// topLevel are the directories a documented path may start with. Anything else in backticks is
// prose, a command, or an environment variable, and is none of this check's business.
var topLevel = []string{
	"api/", "cmd/", "deploy/", "docs/", "internal/", "plugins/", "test/", "tools/", "web/",
}

// pathish matches a token that could be a repository path: inside backticks, or drawn in a tree
// block. Braces and asterisks are kept — they are expanded below, because the brace form is exactly
// where this project's layout trees went wrong.
var pathish = regexp.MustCompile(`[A-Za-z0-9_./*{},\-]+`)

// checkPaths asserts that every repository path the documentation names actually exists.
//
// The failure this exists to prevent is specific and has happened: both layout trees listed
// internal/controlplane/{scheduler,alerting,rbac,auth}/, four packages that were never written,
// for six weeks, in the file a new contributor reads first.
func checkPaths(r *repo) []finding {
	var out []finding

	for _, d := range r.docs {
		for _, c := range claimedPaths(d) {
			for _, p := range expandBraces(c.path) {
				if r.allowed(d.path, p) {
					continue
				}
				if !pathExists(r, p) {
					out = append(out, finding{
						file: d.path, line: c.line + 1,
						msg: fmt.Sprintf("names %q, which does not exist", p),
						hint: "documentation describes what is — if this is planned, say so in " +
							"prose, or add it to docs/.docscheck-allow with a reason",
					})
				}
			}
		}
	}

	// An allowance that no longer matches anything is itself drift: it silences a check for a claim
	// nobody makes any more, and will silence the next one made by accident.
	for _, a := range r.allow {
		if !a.used {
			out = append(out, finding{
				file: "docs/.docscheck-allow", line: 0,
				msg:  fmt.Sprintf("allows %q in %s, which does not name it", a.path, a.file),
				hint: "remove it; a stale allowance silences a future mistake",
			})
		}
	}
	return out
}

// claimedPaths returns every repository path a document asserts exists: those in backticks in
// prose, and those a fenced layout tree draws.
//
// A path inside a fenced *example* is not included. `go build -o bin/x` is an instruction, not a
// claim that bin/x is there.
func claimedPaths(d *doc) []entry {
	var out []entry

	kinds := classify(d.lines)
	for i, line := range d.lines {
		if kinds[i] != proseLine {
			continue
		}
		for _, tok := range candidatePaths(line) {
			out = append(out, entry{path: tok, line: i})
		}
	}

	for _, block := range treeBlocks(d.lines) {
		for _, e := range walkTree(block.body) {
			if hasTopLevelPrefix(e.path + "/") {
				out = append(out, entry{path: e.path, line: block.start + e.line})
			}
		}
	}
	return out
}

// candidatePaths pulls path-shaped tokens out of one line of prose.
func candidatePaths(line string) []string {
	var toks []string
	for _, m := range backticked.FindAllStringSubmatch(line, -1) {
		toks = append(toks, pathish.FindAllString(m[1], -1)...)
	}

	var out []string
	for _, t := range toks {
		t = strings.Trim(t, ".,;:")
		// A token carrying ".." is an elision in prose — `api/proto/.../plugin.proto` — or a
		// parent-directory traversal. Neither is a claim that a path exists.
		if !hasTopLevelPrefix(t) || strings.Contains(t, "..") {
			continue
		}
		out = append(out, strings.TrimSuffix(t, "/"))
	}
	return out
}

var backticked = regexp.MustCompile("`([^`]+)`")

func hasTopLevelPrefix(s string) bool {
	for _, p := range topLevel {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// expandBraces turns "a/{b,c}/d" into "a/b/d" and "a/c/d". One level is enough; the layout trees
// never nest them, and a checker that quietly handles more than the documents contain is a checker
// nobody can predict.
func expandBraces(s string) []string {
	open := strings.Index(s, "{")
	if open < 0 {
		return []string{s}
	}
	closeAt := strings.Index(s[open:], "}")
	if closeAt < 0 {
		return []string{s} // unbalanced: let it fail as a whole, visibly
	}
	closeAt += open

	prefix, suffix := s[:open], s[closeAt+1:]
	var out []string
	for _, alt := range strings.Split(s[open+1:closeAt], ",") {
		if alt = strings.TrimSpace(alt); alt != "" {
			out = append(out, prefix+alt+suffix)
		}
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// pathExists resolves a path, treating one containing "*" as satisfied when the glob matches
// anything. "plugins/*/" is a true statement about the tree even though no such directory exists.
func pathExists(r *repo, p string) bool {
	if strings.Contains(p, "*") {
		matches, err := filepath.Glob(filepath.Join(r.root, filepath.FromSlash(p)))
		return err == nil && len(matches) > 0
	}
	return r.exists(p)
}

// checkLayout asserts the other direction: that every control-plane package is named in a layout
// tree, so a new one cannot arrive without the trees being updated in the same change.
//
// Scoped deliberately to internal/controlplane. It is where packages appear as the roadmap advances
// — a scheduler, then auth and rbac — and where the omission that mattered happened: the trees
// listed four packages that did not exist while silently omitting sandbox, which is central to the
// product. A repository-wide version of this check would be mostly false positives.
func checkLayout(r *repo) []finding {
	const parent = "internal/controlplane"

	trees := map[string]bool{}
	for _, d := range r.docs {
		for _, c := range claimedPaths(d) {
			for _, p := range expandBraces(c.path) {
				trees[p] = true
			}
		}
	}

	var missing []string
	for _, name := range r.dirsUnder(parent) {
		if !trees[parent+"/"+name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []finding{{
		file: "README.md", line: 0,
		msg: fmt.Sprintf("no layout tree mentions %s/{%s}",
			parent, strings.Join(missing, ",")),
		hint: "add it to the trees in README.md and CLAUDE.md — a directory is added by the slice " +
			"that fills it, and the tree is part of that slice",
	}}
}
