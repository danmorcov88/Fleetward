// Command wikigen publishes the repository's documentation to the GitHub wiki.
//
// The repository stays authoritative. A wiki has no pull requests, no required status checks and no
// branch protection, so documentation edited there could not be reviewed or checked — which
// contradicts this project's whole quality argument, in the artefact most people read. So the wiki
// is generated from `docs/`, and `make docs-check` guards the source.
//
// Not a marketplace action, because the off-the-shelf wiki syncs copy files and stop there. Three
// things have to happen that a copy does not do:
//
//   - Links must be rewritten. The wiki is a separate git repository with no access to the source
//     tree, so every relative link to a document, a Go file, or the migration 404s unless rewritten.
//   - The namespace is flat. There are no directories, so `docs/adr/0021-….md` has to become a
//     single page name chosen to still read as a URL.
//   - The page set must be exhaustive. A document added to `docs/` and forgotten is invisible here,
//     which is why the manifest is explicit and an unlisted file is an error.
//
// Usage:
//
//	wikigen -out <dir> [-C <repo root>]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	root := flag.String("C", ".", "repository root")
	out := flag.String("out", ".wiki", "directory to write the wiki into")
	flag.Parse()

	if err := run(*root, *out); err != nil {
		fmt.Fprintf(os.Stderr, "wikigen: %v\n", err)
		os.Exit(1)
	}
}

func run(root, out string) error {
	pages, err := buildManifest(root)
	if err != nil {
		return err
	}

	byPath := make(map[string]page, len(pages))
	for _, p := range pages {
		byPath[p.source] = p
	}

	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}

	var allErrs []error
	written := map[string]bool{}

	for _, p := range pages {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.source))) //nolint:gosec // from the manifest
		if err != nil {
			return fmt.Errorf("reading %s: %w", p.source, err)
		}
		doc := strings.ReplaceAll(string(b), "\r\n", "\n")
		doc = stripGeneratedBanner(doc)

		doc, errs := rewriteLinks(root, doc, p.source, byPath)
		allErrs = append(allErrs, errs...)

		content := sourceNote(p.source) + strings.TrimRight(doc, "\n") + "\n\n" + footer()
		if err := writeFile(out, p.wiki+".md", content); err != nil {
			return err
		}
		written[p.wiki] = true
	}

	for name, content := range map[string]string{
		"Home.md":     home(),
		"_Sidebar.md": sidebar(pages),
		"_Footer.md":  footer(),
	} {
		if err := writeFile(out, name, content); err != nil {
			return err
		}
	}

	// A page that no longer has a source has to be removed, or the wiki accumulates documents that
	// were deleted from the repository months ago and still look current.
	if err := removeStale(out, written); err != nil {
		return err
	}

	if len(allErrs) > 0 {
		sort.Slice(allErrs, func(i, j int) bool { return allErrs[i].Error() < allErrs[j].Error() })
		var b strings.Builder
		fmt.Fprintf(&b, "%d link(s) would break on the wiki:\n", len(allErrs))
		for _, e := range allErrs {
			fmt.Fprintf(&b, "  %v\n", e)
		}
		return fmt.Errorf("%s", b.String())
	}

	fmt.Printf("wikigen: %d pages into %s\n", len(pages)+3, out)
	return nil
}

func writeFile(dir, name, content string) error {
	// LF regardless of platform: the wiki is a git repository, and a checkout from a Windows machine
	// must not rewrite every page.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600) //nolint:gosec // generated output
}

// removeStale deletes wiki pages this run did not produce, leaving anything that is not ours.
func removeStale(out string, written map[string]bool) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	keep := map[string]bool{"Home.md": true, "_Sidebar.md": true, "_Footer.md": true}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || keep[name] {
			continue
		}
		if written[strings.TrimSuffix(name, ".md")] {
			continue
		}
		if err := os.Remove(filepath.Join(out, name)); err != nil {
			return err
		}
		fmt.Printf("  removed %s (no longer in the repository)\n", name)
	}
	return nil
}
