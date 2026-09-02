package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWikiName(t *testing.T) {
	tests := []struct {
		prefix, file, want string
	}{
		{"", "0021-plugins-upload-artifacts-as-multipart-parts.md",
			"ADR-0021-Plugins-Upload-Artifacts-as-Multipart-Parts"},
		{"", "0007-s3-object-storage-for-artifacts.md", "ADR-0007-S3-Object-Storage-for-Artifacts"},
		{"Journal-", "A5-restore-and-verify.md", "Journal-A5-Restore-and-Verify"},
		{"Slice-", "A3-sandbox-provider.md", "Slice-A3-Sandbox-Provider"},
		{"Journal-", "00-foundation.md", "Journal-00-Foundation"},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			if got := wikiName(tc.prefix, tc.file); got != tc.want {
				t.Errorf("wikiName(%q, %q) = %q, want %q", tc.prefix, tc.file, got, tc.want)
			}
		})
	}
}

// TestRewriteTarget covers each rule, because every one of them is a link that silently 404s if it
// is wrong: the wiki is a separate repository and cannot see the source tree at all.
func TestRewriteTarget(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "docs", "adr"))
	mustMkdir(t, filepath.Join(root, "internal", "config"))
	mustWrite(t, filepath.Join(root, "internal", "config", "config.go"), "package config\n")

	byPath := map[string]page{
		"docs/adr/0021-multipart.md": {source: "docs/adr/0021-multipart.md", wiki: "ADR-0021-Multipart"},
		"docs/engines.md":            {source: "docs/engines.md", wiki: "Supported-Engines"},
	}

	tests := []struct {
		name    string
		target  string
		dir     string
		image   bool
		want    string
		wantErr bool
	}{
		{
			name:   "a document becomes its wiki page",
			target: "../adr/0021-multipart.md", dir: "docs/dev",
			want: "ADR-0021-Multipart",
		},
		{
			name:   "an anchor survives",
			target: "engines.md#status", dir: "docs",
			want: "Supported-Engines#status",
		},
		{
			name:   "a source file becomes an absolute blob link",
			target: "../internal/config/config.go", dir: "docs",
			want: blobBase + "internal/config/config.go",
		},
		{
			name:   "a directory becomes a tree link, not a blob link",
			target: "adr/", dir: "docs",
			want: treeBase + "docs/adr",
		},
		{
			name:   "an external link is untouched",
			target: "https://go.dev", dir: "docs",
			want: "https://go.dev",
		},
		{
			name:   "a bare anchor is untouched",
			target: "#the-one-rule", dir: "docs",
			want: "#the-one-rule",
		},
		{
			name:   "an image resolves against raw",
			target: "img/x.png", dir: "docs", image: true,
			want: rawBase + "docs/img/x.png",
		},
		{
			name:   "an unpublished document points at the repository copy",
			target: "../CLAUDE.md", dir: "docs",
			want: blobBase + "CLAUDE.md",
		},
		{
			name:   "a document that is neither published nor excluded is an error",
			target: "nowhere.md", dir: "docs",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteTarget(root, tc.target, tc.dir, byPath, tc.image)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("rewriteTarget(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestRewriteLinksLeavesFencedBlocksAlone guards against mangling an example. A shell block showing
// a markdown link, or a mermaid diagram, is text — rewriting inside it would corrupt the page.
func TestRewriteLinksLeavesFencedBlocksAlone(t *testing.T) {
	root := t.TempDir()
	byPath := map[string]page{"docs/engines.md": {source: "docs/engines.md", wiki: "Supported-Engines"}}

	doc := strings.Join([]string{
		"See [engines](engines.md).",
		"```markdown",
		"See [engines](engines.md).",
		"```",
	}, "\n")

	got, errs := rewriteLinks(root, doc, "docs/x.md", byPath)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	lines := strings.Split(got, "\n")
	if lines[0] != "See [engines](Supported-Engines)." {
		t.Errorf("prose link was not rewritten: %q", lines[0])
	}
	if lines[2] != "See [engines](engines.md)." {
		t.Errorf("link inside a fence was rewritten: %q", lines[2])
	}
}

// TestManifestRejectsAnUnpublishedDocument is the check that makes the wiki exhaustive by
// construction. A file added to docs/ and forgotten would simply not appear, which is the kind of
// failure nobody notices for months.
func TestManifestRejectsAnUnpublishedDocument(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"docs/adr", "docs/dev/journal", "docs/dev/slices"} {
		mustMkdir(t, filepath.Join(root, filepath.FromSlash(d)))
	}
	mustWrite(t, filepath.Join(root, "docs", "brand-new-thing.md"), "# New\n")

	_, err := buildManifest(root)
	if err == nil {
		t.Fatal("expected an error for a document that is neither published nor excluded")
	}
	if !strings.Contains(err.Error(), "brand-new-thing.md") {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestStripGeneratedBanner(t *testing.T) {
	in := "<!-- Generated by tools/docsgen. DO NOT EDIT. -->\n<!-- Source: x -->\n\n# Title\n\nBody.\n"
	got := stripGeneratedBanner(in)
	if !strings.HasPrefix(got, "# Title") {
		t.Errorf("banner not stripped: %q", got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
