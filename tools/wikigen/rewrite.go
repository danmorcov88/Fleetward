package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	repoBase = "https://github.com/danmorcov88/Fleetward/"
	blobBase = repoBase + "blob/main/"
	treeBase = repoBase + "tree/main/"
	rawBase  = "https://raw.githubusercontent.com/danmorcov88/Fleetward/main/"
)

var (
	mdLink    = regexp.MustCompile(`(!?)\[([^\]]*)\]\(([^)\s]+)(\s+"[^"]*")?\)`)
	fenceMark = regexp.MustCompile("^\\s*```")
)

// rewriteLinks turns a document's repository-relative links into links that work on the wiki.
//
// This is the part a naive copy gets wrong, and it fails silently: the wiki is a separate git
// repository with no access to the source tree, so every link that resolves in the repo — to
// another document, to a Go file, to the migration — 404s once published unless it is rewritten.
// The documents are full of them.
func rewriteLinks(root, doc, source string, byPath map[string]page) (string, []error) {
	var errs []error
	dir := path.Dir(source)

	lines := strings.Split(doc, "\n")
	inFence := false
	for i, line := range lines {
		if fenceMark.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue // a link inside an example is text, not navigation
		}

		lines[i] = mdLink.ReplaceAllStringFunc(line, func(m string) string {
			g := mdLink.FindStringSubmatch(m)
			bang, text, target, title := g[1], g[2], g[3], g[4]

			rewritten, err := rewriteTarget(root, target, dir, byPath, bang == "!")
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", source, err))
				return m
			}
			return fmt.Sprintf("%s[%s](%s%s)", bang, text, rewritten, title)
		})
	}
	return strings.Join(lines, "\n"), errs
}

func rewriteTarget(root, target, dir string, byPath map[string]page, isImage bool) (string, error) {
	switch {
	case target == "":
		return target, nil
	case strings.Contains(target, "://"), strings.HasPrefix(target, "mailto:"):
		return target, nil
	case strings.HasPrefix(target, "#"):
		// A link within the page. Wiki headings slug the same way GitHub does elsewhere.
		return target, nil
	}

	file, anchor, hasAnchor := strings.Cut(target, "#")
	resolved := path.Clean(path.Join(dir, file))

	suffix := ""
	if hasAnchor {
		suffix = "#" + anchor
	}

	if isImage {
		return rawBase + resolved, nil
	}

	if p, ok := byPath[resolved]; ok {
		return p.wiki + suffix, nil
	}

	if strings.HasSuffix(resolved, ".md") {
		// A document deliberately left in the repository still exists there, so the link points at
		// the repository copy. That is what "not published" means, and it is not an error.
		if _, excluded := notPublished[resolved]; excluded {
			return blobBase + resolved + suffix, nil
		}
		// A document that is neither published nor deliberately excluded would 404. Failing here is
		// the whole reason the manifest is explicit: it makes "every document is reachable" a
		// checked property rather than an intention.
		return "", fmt.Errorf("links to %q, which is neither a wiki page nor in notPublished — "+
			"add it to tools/wikigen/manifest.go", resolved)
	}

	// Anything else in the repository — a Go file, the migration, a workflow — becomes an absolute
	// link to the repository, because the wiki cannot see the tree. A directory needs tree/ rather
	// than blob/; GitHub 404s a blob URL that names a directory, which is how a link to `docs/adr/`
	// looks fine in review and is broken in the published page.
	base := blobBase
	if isDir(root, resolved) {
		base = treeBase
	}
	return base + resolved + suffix, nil
}

func isDir(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

// stripGeneratedBanner removes the "DO NOT EDIT" comment from generated documents. On the wiki it
// is noise: nobody reading the configuration reference there can edit it anyway, and the footer
// already says where every page comes from.
func stripGeneratedBanner(doc string) string {
	lines := strings.Split(doc, "\n")
	cut := 0
	for cut < len(lines) && (strings.HasPrefix(lines[cut], "<!--") || strings.TrimSpace(lines[cut]) == "") {
		cut++
	}
	if cut == 0 {
		return doc
	}
	return strings.Join(lines[cut:], "\n")
}
