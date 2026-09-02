// Command docscheck is the merge gate for documentation claims.
//
// This project's quality argument is that a claim is trustworthy because a merge gate enforces it:
// buf breaking guards the contract, the conformance suite guards plugins, a versioned ruleset
// guards main. Documentation had no such gate, and by the time anyone counted, seventeen statements
// in it were verifiably false — packages that were never written, a tool that was never adopted, a
// security policy describing an authorization layer that does not exist.
//
// None of those were careless. They were written from the architecture rather than from the code,
// which reads as accurate right up until someone checks. This command is that someone.
//
// It checks only what can be checked mechanically. "This prose is still true" cannot be, and lives
// as a line in the pull request template instead.
//
// Usage:
//
//	docscheck [-C dir] [-v]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// checks are run in the order listed, and all of them run even when an earlier one fails: someone
// fixing documentation wants the whole list, not the first item repeatedly.
var checks = []check{
	{"links", "every relative markdown link resolves", checkLinks},
	{"paths", "every repository path named in the docs exists", checkPaths},
	{"layout", "every control-plane package appears in a layout tree", checkLayout},
	{"adr", "decision records follow the template and their supersessions agree", checkADRs},
	{"ci", "the CI job set matches the ruleset and the prose", checkCI},
	{"vocabulary", "retired vocabulary and non-English prose stay out", checkVocabulary},
}

type check struct {
	name string
	what string
	run  func(*repo) []finding
}

// finding is one thing that is wrong, located precisely enough to fix without searching.
type finding struct {
	file string
	line int // 1-indexed; 0 when the finding is about the file as a whole
	msg  string
	hint string // what to do about it, when that is not obvious from msg
}

func (f finding) String() string {
	loc := f.file
	if f.line > 0 {
		loc = fmt.Sprintf("%s:%d", f.file, f.line)
	}
	s := fmt.Sprintf("%s: %s", loc, f.msg)
	if f.hint != "" {
		s += "\n      " + f.hint
	}
	return s
}

func main() {
	dir := flag.String("C", ".", "run as if docscheck had been started in `dir`")
	verbose := flag.Bool("v", false, "list every check, including those that pass")
	flag.Parse()

	r, err := loadRepo(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "docscheck: %v\n", err)
		os.Exit(2)
	}

	total := 0
	for _, c := range checks {
		found := c.run(r)
		sort.Slice(found, func(i, j int) bool {
			if found[i].file != found[j].file {
				return found[i].file < found[j].file
			}
			return found[i].line < found[j].line
		})

		switch {
		case len(found) > 0:
			fmt.Printf("FAIL  %-11s %s\n", c.name, c.what)
			for _, f := range found {
				fmt.Printf("    %s\n", f)
			}
			total += len(found)
		case *verbose:
			fmt.Printf("ok    %-11s %s\n", c.name, c.what)
		}
	}

	if total > 0 {
		fmt.Printf("\n%d problem(s) in the documentation.\n", plural(total))
		fmt.Println("Documentation describes what is; slice briefs describe what will be.")
		os.Exit(1)
	}
	fmt.Printf("docscheck: %d markdown files, no problems\n", len(r.docs))
}

func plural(n int) int { return n }

// repo is the checked-out tree, loaded once so that every check reads the same bytes.
type repo struct {
	root string
	docs []*doc

	// allow lists the paths the docs may name despite their not existing.
	allow []allowance
}

// allowance permits one document to name one path that does not exist.
//
// Scoped to a file, not global. An allowance exists because the surrounding prose makes the mention
// honest — "planned, gated on the Kubernetes sandbox provider" — and that sentence lives in one
// document. A global allowance would silently extend it to every other file, which is how the same
// claim reappears somewhere it is not qualified.
type allowance struct {
	path   string
	file   string // the only document allowed to name it
	reason string
	used   bool
}

// doc is one markdown file.
type doc struct {
	path  string // slash-separated, relative to the repository root
	lines []string
}

// text returns the whole file, for checks that are easier to write against one string.
func (d *doc) text() string { return strings.Join(d.lines, "\n") }

// skipDirs are not ours to check, or are build output.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "bin": true, "dist": true, "api": true,
}

func loadRepo(root string) (*repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	r := &repo{root: abs}

	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // walking a directory the caller named
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		r.docs = append(r.docs, &doc{
			path:  filepath.ToSlash(rel),
			lines: strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n"),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", abs, err)
	}
	if len(r.docs) == 0 {
		return nil, fmt.Errorf("no markdown found under %s — wrong directory?", abs)
	}
	sort.Slice(r.docs, func(i, j int) bool { return r.docs[i].path < r.docs[j].path })

	if err := r.loadAllowList(); err != nil {
		return nil, err
	}
	return r, nil
}

// loadAllowList reads docs/.docscheck-allow, whose every entry must carry a reason.
func (r *repo) loadAllowList() error {
	name := filepath.Join(r.root, "docs", ".docscheck-allow")
	b, err := os.ReadFile(name) //nolint:gosec // a fixed path inside the repository
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spec, reason, ok := strings.Cut(line, "#")
		reason = strings.TrimSpace(reason)
		fields := strings.Fields(spec)
		if len(fields) != 2 {
			return fmt.Errorf("docs/.docscheck-allow:%d: expected `<path> <file>  # <reason>`, got %q",
				i+1, strings.TrimSpace(spec))
		}
		if !ok || reason == "" {
			return fmt.Errorf("docs/.docscheck-allow:%d: %q has no reason after '#'; "+
				"an allowance without a reason is just a silenced error", i+1, fields[0])
		}
		r.allow = append(r.allow, allowance{path: fields[0], file: fields[1], reason: reason})
	}
	return nil
}

// allowed reports whether doc may name path, and records that the allowance was used.
func (r *repo) allowed(docPath, p string) bool {
	for i := range r.allow {
		if r.allow[i].path == p && r.allow[i].file == docPath {
			r.allow[i].used = true
			return true
		}
	}
	return false
}

// exists reports whether a repository-relative path is present on disk.
func (r *repo) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(rel)))
	return err == nil
}

// dirsUnder lists the immediate subdirectories of a repository-relative directory.
func (r *repo) dirsUnder(rel string) []string {
	entries, err := os.ReadDir(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// readFile returns a non-markdown file the checks need, such as a workflow or the ruleset.
func (r *repo) readFile(rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(r.root, filepath.FromSlash(rel))) //nolint:gosec // fixed paths
}

// lineKind says what sort of line this is, which decides how much weight to give what it says.
type lineKind int

const (
	proseLine lineKind = iota // outside any fence: a claim
	treeLine                  // inside a fenced directory tree: also a claim
	codeLine                  // inside any other fence: an example, not a claim
)

// classify labels every line of a document.
//
// The distinction that matters is between a fenced *tree* and a fenced *example*. A path in a shell
// example is an instruction — `go build -o bin/x` does not assert that bin/x exists — while a path
// in a layout tree is a statement about the repository, and is where this project's documentation
// actually went wrong.
//
// A fence is a tree when it carries no language tag and either draws itself with box characters, as
// README.md does, or opens on a directory line, as CLAUDE.md does. Recognising only the first form
// is a mistake worth naming: it silently skipped one of the two trees this command exists to check.
func classify(lines []string) []lineKind {
	out := make([]lineKind, len(lines))

	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		info := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))

		// Find the end of this block.
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				end = j
				break
			}
		}

		kind := codeLine
		if info == "" && looksLikeTree(lines[i+1:end]) {
			kind = treeLine
		}
		for j := i + 1; j < end; j++ {
			out[j] = kind
		}
		i = end // the closing fence is neither in nor out
	}
	return out
}

// treeBlock is one fenced directory tree, with the document line its body starts on.
type treeBlock struct {
	start int // 0-indexed line of body[0] within the document
	body  []string
}

// treeBlocks returns every fenced directory tree in a document.
func treeBlocks(lines []string) []treeBlock {
	var out []treeBlock
	kinds := classify(lines)
	for i := 0; i < len(lines); i++ {
		if kinds[i] != treeLine {
			continue
		}
		j := i
		for j < len(lines) && kinds[j] == treeLine {
			j++
		}
		out = append(out, treeBlock{start: i, body: lines[i:j]})
		i = j
	}
	return out
}

func looksLikeTree(body []string) bool {
	for _, l := range body {
		if strings.ContainsAny(l, "├└│") {
			return true
		}
	}
	for _, l := range body {
		if strings.TrimSpace(l) == "" {
			continue
		}
		// The first content line of a tree is its root directory.
		return strings.HasSuffix(strings.TrimSpace(l), "/")
	}
	return false
}
