package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandBraces(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"internal/config", []string{"internal/config"}},
		{"docs/{adr,dev}", []string{"docs/adr", "docs/dev"}},
		{
			"internal/controlplane/{api,inventory,backup,sandbox}/",
			[]string{
				"internal/controlplane/api/",
				"internal/controlplane/inventory/",
				"internal/controlplane/backup/",
				"internal/controlplane/sandbox/",
			},
		},
		{"internal/plugin/{manager,sdk}/x", []string{"internal/plugin/manager/x", "internal/plugin/sdk/x"}},
		// Unbalanced input is returned whole rather than guessed at, so it fails visibly.
		{"docs/{adr", []string{"docs/{adr"}},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := expandBraces(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("expandBraces(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("expandBraces(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCandidatePaths covers prose only; a tree is resolved by walkTree, which has its own test.
func TestCandidatePaths(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "backticked path",
			line: "The scheduler will live in `internal/controlplane/scheduler`.",
			want: []string{"internal/controlplane/scheduler"},
		},
		{
			name: "a command is not a path",
			line: "Run `pg_dump --version` and `make build`.",
			want: nil,
		},
		{
			name: "an elision is not a path",
			line: "- `api/proto/.../plugin.proto` — one additive message",
			want: nil,
		},
		{
			name: "a parent traversal is not a claim about the tree",
			line: "See `../adr/0021-plugins-upload-artifacts-as-multipart-parts.md`.",
			want: nil,
		},
		{
			name: "trailing punctuation is trimmed",
			line: "It lives at `internal/config`, shared by both.",
			want: []string{"internal/config"},
		},
		{
			name: "two paths on one line",
			line: "Both `internal/config` and `web/src` changed.",
			want: []string{"internal/config", "web/src"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := candidatePaths(tc.line)
			if len(got) != len(tc.want) {
				t.Fatalf("candidatePaths(%q) = %v, want %v", tc.line, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestIsMention guards the use/mention distinction. Without it the check cannot tell a document
// that retires a label from one that still uses it, and the only remedy is a growing list of
// per-file exemptions — which is how a checker becomes something people disable.
func TestIsMention(t *testing.T) {
	tests := []struct {
		line  string
		match string
		want  bool
	}{
		{`Those labels are retired: "Phase F" no longer exists.`, "Phase F", true},
		{"The `Phase F` label is gone.", "Phase F", true},
		{"Phase F covers production readiness.", "Phase F", false},
		{"Everything else waits for Phase F.", "Phase F", false},
		{`Read "Stage 3" as "a later slice".`, "Stage 3", true},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			if got := isMention(tc.line, tc.match); got != tc.want {
				t.Errorf("isMention(%q, %q) = %v, want %v", tc.line, tc.match, got, tc.want)
			}
		})
	}
}

// TestClassify covers the distinction the whole paths check rests on. Recognising a tree only by
// its box-drawing characters silently skipped CLAUDE.md's tree, which draws itself with plain
// indentation — so the check passed while the file it most needed to read went unexamined. The
// last two cases are the ones that would have caught that.
func TestClassify(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []lineKind
	}{
		{
			name:  "a tagged fence is code",
			lines: []string{"prose", "```go", "x := 1", "```", "after"},
			want:  []lineKind{proseLine, proseLine, codeLine, proseLine, proseLine},
		},
		{
			name:  "an untagged shell example is code, not a tree",
			lines: []string{"```", "make build", "```"},
			want:  []lineKind{proseLine, codeLine, proseLine},
		},
		{
			name:  "a box-drawn tree is a tree",
			lines: []string{"```", "fleetward/", "├── api/", "```"},
			want:  []lineKind{proseLine, treeLine, treeLine, proseLine},
		},
		{
			name:  "an indented tree with no drawing is still a tree",
			lines: []string{"```", "fleetward/", "  internal/", "    config/", "```"},
			want:  []lineKind{proseLine, treeLine, treeLine, treeLine, proseLine},
		},
		{
			name:  "two fences in one document",
			lines: []string{"```", "fleetward/", "```", "between", "```bash", "ls", "```"},
			want: []lineKind{
				proseLine, treeLine, proseLine, proseLine, proseLine, codeLine, proseLine,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.lines)
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("line %d (%q): kind = %d, want %d", i, tc.lines[i], got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAllowListDemandsAReason is the check on the escape hatch. An allowance with no reason is
// indistinguishable from a silenced error six months later, so loading one is a hard failure rather
// than a warning.
func TestAllowListDemandsAReason(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"reason given", "deploy/helm CLAUDE.md # planned, gated on the k8s provider\n", false},
		{"no reason", "deploy/helm CLAUDE.md\n", true},
		{"empty reason", "deploy/helm CLAUDE.md #   \n", true},
		{"no file scope", "deploy/helm # planned\n", true},
		{"comments and blanks are fine", "# a header\n\n  \ndocs/x README.md # because\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "docs", ".docscheck-allow"), tc.content)
			mustWrite(t, filepath.Join(root, "README.md"), "# x\n")

			_, err := loadRepo(root)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error for an allowance with no reason")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestCheckPathsFindsAPackageThatDoesNotExist reproduces the failure this command was written for:
// a layout tree naming four packages that were never written.
func TestCheckPathsFindsAPackageThatDoesNotExist(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "internal", "controlplane", "api", "keep.go"), "package api\n")
	mustWrite(t, filepath.Join(root, "README.md"), strings.Join([]string{
		"# Layout",
		"",
		"```",
		"  internal/controlplane/{api,scheduler}/",
		"```",
		"",
	}, "\n"))

	r, err := loadRepo(root)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}

	found := checkPaths(r)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(found), found)
	}
	if !strings.Contains(found[0].msg, "internal/controlplane/scheduler") {
		t.Errorf("finding does not name the missing package: %s", found[0].msg)
	}
}

// TestCheckLayoutFindsAPackageMissingFromTheTree is the other direction, and the one that fires
// when a slice adds a package: sandbox existed for weeks while both trees omitted it.
func TestCheckLayoutFindsAPackageMissingFromTheTree(t *testing.T) {
	root := t.TempDir()
	for _, pkg := range []string{"api", "sandbox"} {
		mustWrite(t, filepath.Join(root, "internal", "controlplane", pkg, "x.go"), "package x\n")
	}
	mustWrite(t, filepath.Join(root, "README.md"), "# Layout\n\n```\n  internal/controlplane/api/\n```\n")

	r, err := loadRepo(root)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}

	found := checkLayout(r)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(found), found)
	}
	if !strings.Contains(found[0].msg, "sandbox") {
		t.Errorf("finding does not name the missing package: %s", found[0].msg)
	}
}

func TestCheckADRRequiresTheAlternatives(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "docs", "adr", "0001-a-decision.md"), strings.Join([]string{
		"# ADR-0001: A decision",
		"",
		"- **Status:** Accepted",
		"- **Date:** 2026-01-01",
		"",
		"## Context",
		"## Decision",
		"## Consequences",
		"",
	}, "\n"))

	r, err := loadRepo(root)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}

	var got []string
	for _, f := range checkADRs(r) {
		got = append(got, f.msg)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Alternatives considered") {
		t.Errorf("missing alternatives section was not reported; got:\n%s", joined)
	}
	if !strings.Contains(joined, "there is no ADR index") {
		t.Errorf("missing index was not reported; got:\n%s", joined)
	}
}

// TestSupersessionMustBeMutual reproduces the relationship the corpus described in prose but never
// modelled: ADR-0021 replaced ADR-0007's upload mechanism, and 0007 said nothing about it.
func TestSupersessionMustBeMutual(t *testing.T) {
	root := t.TempDir()
	adr := func(n, title, body string) {
		mustWrite(t, filepath.Join(root, "docs", "adr", n+"-"+title+".md"), strings.Join([]string{
			"# ADR-" + n + ": " + title,
			"",
			"- **Status:** Accepted",
			"- **Date:** 2026-01-01",
			body,
			"",
			"## Context",
			"## Decision",
			"## Consequences",
			"## Alternatives considered",
			"",
		}, "\n"))
	}
	adr("0001", "first", "")
	adr("0002", "second", "- **Supersedes:** ADR-0001")
	mustWrite(t, filepath.Join(root, "docs", "adr", "README.md"),
		"# Index\n\n| [0001](0001-first.md) | x | y |\n| [0002](0002-second.md) | x | y |\n")

	r, err := loadRepo(root)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}

	found := checkADRs(r)
	var reported bool
	for _, f := range found {
		if strings.Contains(f.msg, "superseded by ADR-0002 but does not say so") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("one-sided supersession was not reported; got %v", found)
	}
}

func TestDifference(t *testing.T) {
	got := difference([]string{"a", "b", "c"}, []string{"b"})
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("difference = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWalkTree covers path resolution through an indentation-nested tree. Both of this project's
// layout trees nest this way, and the first version of this command only understood absolute paths
// — so it read almost none of them and reported success. Every case here is drawn from a real line.
func TestWalkTree(t *testing.T) {
	tests := []struct {
		name string
		body []string
		want []string
	}{
		{
			name: "box-drawn, nested",
			body: []string{
				"fleetward/",
				"├── api/",
				"│   ├── proto/fleetward/v1/   # the contract",
				"│   └── gen/                  # generated Go",
			},
			want: []string{"api", "api/proto/fleetward/v1", "api/gen"},
		},
		{
			name: "indented, no drawing — the form that was silently skipped",
			body: []string{
				"fleetward/",
				"  internal/",
				"    config/                   # shared by the server and the CLI",
				"    controlplane/{api,backup}/",
			},
			want: []string{
				"internal", "internal/config", "internal/controlplane/{api,backup}",
			},
		},
		{
			name: "a braced parent hands its own name to its children, not each alternative",
			body: []string{
				"fleetward/",
				"  internal/",
				"    storage/{metadb,tsdb}/",
				"      metadb/migrations/      # go:embed-ed into the binary",
			},
			want: []string{
				"internal", "internal/storage/{metadb,tsdb}", "internal/storage/metadb/migrations",
			},
		},
		{
			name: "several siblings on one line are leaves, and none becomes the parent",
			body: []string{
				"fleetward/",
				"  .github/",
				"    CONTRIBUTING.md  SECURITY.md",
				"    workflows/",
			},
			want: []string{
				".github", ".github/CONTRIBUTING.md", ".github/SECURITY.md", ".github/workflows",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, e := range walkTree(tc.body) {
				got = append(got, e.path)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("walkTree = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAllowanceIsScopedToOneFile guards the hole this file-scoping closed: a global allowance
// granted because one document qualifies the mention would silently permit every other document to
// make the same claim unqualified.
func TestAllowanceIsScopedToOneFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "docs", ".docscheck-allow"),
		"internal/planned  CLAUDE.md  # planned, and the sentence around it says so\n")
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "It will live at `internal/planned`.\n")
	mustWrite(t, filepath.Join(root, "README.md"), "It lives at `internal/planned`.\n")

	r, err := loadRepo(root)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}

	found := checkPaths(r)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want exactly 1 (README only): %v", len(found), found)
	}
	if found[0].file != "README.md" {
		t.Errorf("finding is against %s, want README.md", found[0].file)
	}
}
