package main

import "strings"

// entry is one path a tree block claims exists, and the line that claims it.
type entry struct {
	path string // repository-relative, with braces still unexpanded
	line int    // 0-indexed, relative to the block body
}

// walkTree resolves a fenced directory tree into repository-relative paths.
//
// Both of this project's trees nest by indentation rather than repeating the parent, so a line
// reading "controlplane/{api,inventory}/" is a claim about internal/controlplane/api only once it
// is read together with the "internal/" line above it. A checker that only understood absolute
// paths skipped every such line — which is to say, almost the entire tree — and reported success.
//
// The rule for descending is deliberately strict: a line pushes a new parent only when it holds a
// single directory entry. "ISSUE_TEMPLATE/  PULL_REQUEST_TEMPLATE.md  dependabot.yml" lists three
// siblings, and guessing which of them the following lines belong under would be a checker whose
// behaviour nobody can predict from reading the tree.
func walkTree(body []string) []entry {
	type frame struct {
		indent int
		prefix string
	}
	var stack []frame
	var out []entry

	for i, raw := range body {
		line, indent := stripDrawing(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if c := strings.Index(line, "#"); c >= 0 {
			line = line[:c]
		}
		tokens := strings.Fields(line)
		if len(tokens) == 0 {
			continue
		}

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		prefix := ""
		if len(stack) > 0 {
			prefix = stack[len(stack)-1].prefix
		}

		// The root line names the repository, not a directory inside it.
		if len(stack) == 0 && len(tokens) == 1 && isRepoRoot(tokens[0]) {
			stack = append(stack, frame{indent: indent, prefix: ""})
			continue
		}

		for _, t := range tokens {
			full := prefix + strings.TrimSuffix(t, "/")
			if full != "" {
				out = append(out, entry{path: full, line: i})
			}
		}

		if len(tokens) == 1 && strings.HasSuffix(tokens[0], "/") {
			// A braced entry names several siblings inside one parent, so what deeper lines hang
			// off is the part before the brace. "storage/{metadb,tsdb}/" followed by an indented
			// "metadb/migrations/" means internal/storage/metadb/migrations, not one path per
			// alternative — reading it the other way produced four confident nonsense paths.
			child := tokens[0]
			if b := strings.Index(child, "{"); b >= 0 {
				child = child[:b]
			}
			stack = append(stack, frame{indent: indent, prefix: prefix + child})
		}
	}
	return out
}

// stripDrawing replaces box-drawing characters with spaces so that indentation still measures
// depth, and returns the cleaned line with its indent width.
func stripDrawing(s string) (string, int) {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune("├└│─", r) {
			return ' '
		}
		return r
	}, s)

	indent := 0
	for _, r := range cleaned {
		if r != ' ' && r != '\t' {
			break
		}
		indent++
	}
	return cleaned, indent
}

// isRepoRoot reports whether a tree's first line names the repository itself rather than a
// directory within it. Both trees open with "fleetward/".
func isRepoRoot(token string) bool {
	return strings.TrimSuffix(token, "/") == "fleetward"
}
