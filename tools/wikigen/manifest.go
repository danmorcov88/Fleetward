package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// audience groups the sidebar. A wiki without one is a list of files; with one it is a place a
// reader can arrive at knowing what they want and leave having found it.
type audience struct {
	key   string
	title string
	blurb string
}

var audiences = []audience{
	{"evaluate", "Evaluate", "What it is, and whether it is for you"},
	{"run", "Run it", "Installing and operating it"},
	{"extend", "Extend it", "Writing a plugin for your engine"},
	{"understand", "Understand it", "Why it is built this way"},
}

// page is one wiki page and the repository file it is generated from.
type page struct {
	source   string // repository-relative
	wiki     string // wiki page name, without .md
	title    string // sidebar label
	audience string
	hidden   bool // published and linkable, but not listed in the sidebar
}

// explicit are the pages whose names are chosen rather than derived. A wiki page name is a URL and
// a heading in someone's sidebar, so `Operating-Fleetward` is worth typing out where a mechanical
// `docs-ops-operating` would not be.
var explicit = []page{
	{source: "docs/why.md", wiki: "Why-Fleetward", title: "Why Fleetward", audience: "evaluate"},
	{source: "docs/architecture.md", wiki: "Architecture", title: "Architecture", audience: "evaluate"},
	{source: "docs/engines.md", wiki: "Supported-Engines", title: "Supported engines", audience: "evaluate"},
	{source: "docs/roadmap.md", wiki: "Roadmap", title: "Roadmap", audience: "evaluate"},

	{source: "docs/ops/configuration.md", wiki: "Configuration-Reference", title: "Configuration reference", audience: "run"},
	{source: "docs/ops/scheduling.md", wiki: "Scheduling", title: "Scheduling", audience: "run"},

	{source: "docs/dev/writing-an-engine-plugin.md", wiki: "Writing-an-Engine-Plugin", title: "Writing an engine plugin", audience: "extend"},
	{source: ".github/CONTRIBUTING.md", wiki: "Contributing", title: "Contributing", audience: "extend"},
	{source: ".github/SECURITY.md", wiki: "Security-Policy", title: "Security policy", audience: "extend"},
	{source: ".github/CODE_OF_CONDUCT.md", wiki: "Code-of-Conduct", title: "Code of conduct", audience: "extend", hidden: true},

	{source: "docs/dev/design-notes.md", wiki: "Design-Notes", title: "Design notes", audience: "understand"},
	{source: "docs/dev/data-model.md", wiki: "Data-Model", title: "Data model", audience: "understand"},
	{source: "docs/adr/README.md", wiki: "Decision-Records", title: "Decision records", audience: "understand"},
	{source: "docs/dev/STATUS.md", wiki: "Project-Status", title: "Project status", audience: "understand"},
	{source: "docs/dev/journal/README.md", wiki: "Engineering-Journal", title: "Engineering journal", audience: "understand"},
	{source: "docs/dev/slices/README.md", wiki: "Slice-Briefs", title: "Slice briefs", audience: "understand"},
}

// generated are directories whose every file becomes a page, named mechanically. There are
// thirty-seven of them between the records, the journal, and the briefs, and hand-naming those
// would be a manifest nobody keeps current.
var generated = []struct {
	dir      string
	prefix   string
	audience string
}{
	{"docs/adr", "", "understand"},
	{"docs/dev/journal", "Journal-", "understand"},
	{"docs/dev/slices", "Slice-", "understand"},
}

// notPublished are files deliberately left in the repository.
var notPublished = map[string]string{
	"CLAUDE.md":                        "instructions addressed to an AI session; it reads strangely as a document, and links to it resolve to the repository copy",
	"README.md":                        "GitHub renders it on the repository front page already; Home says the same thing for the wiki",
	".github/PULL_REQUEST_TEMPLATE.md": "a form, not a document",
}

var adrFile = regexp.MustCompile(`^(\d{4})-(.+)\.md$`)

// buildManifest returns every page to publish, and fails on any markdown under docs/ that is
// neither published nor explicitly excluded.
//
// Exhaustive by construction rather than by discipline: a file added to docs/ and forgotten is
// invisible on the wiki, which is the quiet failure this check exists to prevent.
func buildManifest(root string) ([]page, error) {
	pages := append([]page(nil), explicit...)
	claimed := map[string]bool{}
	for _, p := range pages {
		claimed[p.source] = true
	}

	for _, g := range generated {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(g.dir)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", g.dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") {
				continue
			}
			source := g.dir + "/" + name
			if claimed[source] {
				continue
			}
			claimed[source] = true
			pages = append(pages, page{
				source:   source,
				wiki:     wikiName(g.prefix, name),
				title:    "",   // listed by its own index page, not the sidebar
				hidden:   true, // otherwise the sidebar is thirty-seven links long
				audience: g.audience,
			})
		}
	}

	// Anything under docs/ that is neither published nor excluded is a mistake.
	var orphans []string
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		if !claimed[slash] {
			if _, excluded := notPublished[slash]; !excluded {
				orphans = append(orphans, slash)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return nil, fmt.Errorf("these files are neither published nor excluded, so they would be "+
			"invisible on the wiki: %s\nAdd them to explicit/generated in tools/wikigen/manifest.go, "+
			"or to notPublished with a reason", strings.Join(orphans, ", "))
	}

	sort.Slice(pages, func(i, j int) bool { return pages[i].wiki < pages[j].wiki })
	return pages, nil
}

// wikiName turns a filename into a wiki page name. An ADR keeps its number in front so the index
// reads in order and a link written as ADR-0021 lands where a reader expects.
func wikiName(prefix, filename string) string {
	base := strings.TrimSuffix(filename, ".md")
	if m := adrFile.FindStringSubmatch(filename); m != nil {
		return "ADR-" + m[1] + "-" + titleSlug(m[2])
	}
	return prefix + titleSlug(base)
}

// titleSlug turns "restore-and-verify" into "Restore-and-Verify": readable in a URL, and stable, so
// a link does not break when a page is regenerated.
func titleSlug(s string) string {
	small := map[string]bool{
		"a": true, "an": true, "and": true, "as": true, "at": true, "by": true, "for": true,
		"from": true, "in": true, "into": true, "of": true, "on": true, "or": true, "the": true,
		"to": true, "with": true, "not": true, "is": true, "are": true, "it": true, "its": true,
		"that": true, "what": true, "cannot": true,
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i > 0 && small[strings.ToLower(p)] {
			parts[i] = strings.ToLower(p)
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "-")
}
