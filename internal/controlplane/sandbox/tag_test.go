package sandbox

import (
	"errors"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                      string
		raw                       string
		major, minor, patch, full string
	}{
		{name: "major only", raw: "16", major: "16", full: "16"},
		{name: "major and minor", raw: "16.2", major: "16", minor: "2", full: "16.2"},
		{name: "three components", raw: "8.0.35", major: "8", minor: "0", patch: "35", full: "8.0.35"},
		{name: "vendor suffix", raw: "8.0.35-MariaDB", major: "8", minor: "0", patch: "35", full: "8.0.35-MariaDB"},
		{name: "leading v", raw: "v7.2.4", major: "7", minor: "2", patch: "4", full: "v7.2.4"},
		{name: "surrounding space", raw: "  16.2  ", major: "16", minor: "2", full: "16.2"},
		{name: "not numeric at all", raw: "stable", full: "stable"},
		{name: "empty", raw: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ParseVersion(tc.raw)
			if got.Full != tc.full {
				t.Errorf("Full = %q, want %q", got.Full, tc.full)
			}
			if got.Major != tc.major {
				t.Errorf("Major = %q, want %q", got.Major, tc.major)
			}
			if got.Minor != tc.minor {
				t.Errorf("Minor = %q, want %q", got.Minor, tc.minor)
			}
			if got.Patch != tc.patch {
				t.Errorf("Patch = %q, want %q", got.Patch, tc.patch)
			}
		})
	}
}

func TestResolveTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template *fwv1.SandboxTemplate
		version  string
		want     string
		wantErr  error
	}{
		{
			name:     "the template wins over the default",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Major }}"},
			version:  "15.6",
			want:     "15",
		},
		{
			name:     "a composed tag",
			template: &fwv1.SandboxTemplate{DefaultTag: "8", TagTemplate: "{{ .Major }}.{{ .Minor }}"},
			version:  "8.0.35",
			want:     "8.0",
		},
		{
			name:     "an unknown version falls back to the default",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Major }}"},
			want:     "16",
		},
		{
			name:     "a template rendering to nothing falls back to the default",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Patch }}"},
			version:  "16.2",
			want:     "16",
		},
		{
			name:     "no template at all uses the default",
			template: &fwv1.SandboxTemplate{DefaultTag: "7-alpine"},
			version:  "7.2.4",
			want:     "7-alpine",
		},
		{
			// Verification against the wrong engine is worse than no verification, so there is
			// deliberately no implicit "latest".
			name:     "neither source produces a tag",
			template: &fwv1.SandboxTemplate{TagTemplate: "{{ .Major }}"},
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:     "a tag that Docker would reject",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Full }}"},
			version:  "16.2 (Debian 16.2-1)",
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:     "an unparseable template",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Major"},
			version:  "16",
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:     "a template referencing a field that does not exist",
			template: &fwv1.SandboxTemplate{DefaultTag: "16", TagTemplate: "{{ .Release }}"},
			version:  "16",
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:    "no template",
			wantErr: ErrNoTemplate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveTag(tc.template, tc.version)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("tag = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template *fwv1.SandboxTemplate
		version  string
		want     string
		wantErr  error
	}{
		{
			name:     "official image",
			template: &fwv1.SandboxTemplate{ImageRepository: "postgres", DefaultTag: "16", TagTemplate: "{{ .Major }}"},
			version:  "16.2",
			want:     "postgres:16",
		},
		{
			name:     "namespaced image",
			template: &fwv1.SandboxTemplate{ImageRepository: "docker.io/library/mysql", DefaultTag: "8"},
			want:     "docker.io/library/mysql:8",
		},
		{
			name:     "registry with a port",
			template: &fwv1.SandboxTemplate{ImageRepository: "registry.internal:5000/db/postgres", DefaultTag: "16"},
			want:     "registry.internal:5000/db/postgres:16",
		},
		{
			// A repository smuggling its own tag would let a plugin decide the version core thinks
			// it is verifying against.
			name:     "a repository carrying a tag",
			template: &fwv1.SandboxTemplate{ImageRepository: "postgres:15", DefaultTag: "16"},
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:     "a repository carrying a digest",
			template: &fwv1.SandboxTemplate{ImageRepository: "postgres@sha256:abc", DefaultTag: "16"},
			wantErr:  ErrInvalidTemplate,
		},
		{
			name:     "an empty repository",
			template: &fwv1.SandboxTemplate{DefaultTag: "16"},
			wantErr:  ErrInvalidTemplate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ImageRef(tc.template, tc.version)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}
