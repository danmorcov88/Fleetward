package sandbox

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Version is what a template's tag_template is evaluated against.
//
// The fields are strings rather than integers because that is what a tag is made of, and because
// an engine whose version is "8.0.35-MariaDB" or "2024.1" must still produce something.
type Version struct {
	// Full is the version exactly as Discover reported it.
	Full string
	// Major, Minor, and Patch are the leading numeric components, empty when absent.
	Major string
	Minor string
	Patch string
}

// versionPattern matches the leading numeric components of a version string. Anything after them —
// a build suffix, a vendor name, a pre-release marker — is deliberately ignored: it belongs in Full
// for a template that wants it, and it must never end up in a tag by accident.
var versionPattern = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// ParseVersion splits a reported engine version into the parts a tag template can use.
func ParseVersion(raw string) Version {
	v := Version{Full: strings.TrimSpace(raw)}
	match := versionPattern.FindStringSubmatch(v.Full)
	if match == nil {
		return v
	}
	v.Major, v.Minor, v.Patch = match[1], match[2], match[3]
	return v
}

// tagPattern is Docker's own rule for a legal tag.
var tagPattern = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)

// repositoryPattern is deliberately narrower than Docker's: a registry host, path segments, and
// nothing that could smuggle a tag or a digest in. The repository comes from a plugin binary, and
// while plugins are trusted enough to be launched at all, an image reference assembled from two
// sources is the wrong place to find that out.
//
// A port is only permitted on a leading component that is followed by a path, which is what keeps
// "registry.internal:5000/db/postgres" legal while rejecting "postgres:15" — a repository that
// smuggles in its own tag would let a plugin decide which version core believes it verified.
var repositoryPattern = regexp.MustCompile(
	`^(?:[a-zA-Z0-9][a-zA-Z0-9._-]*(?::\d+)?/)*[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ResolveTag picks the image tag for a sandbox of the given engine version.
//
// The template's tag_template wins when it renders to something usable; default_tag is the
// fallback for a version that cannot be matched. There is no third fallback: pulling "latest" to
// restore a backup would silently verify against the wrong engine, which is the one failure this
// whole slice exists to make impossible.
func ResolveTag(tmpl *fwv1.SandboxTemplate, engineVersion string) (string, error) {
	if tmpl == nil {
		return "", ErrNoTemplate
	}

	tag := strings.TrimSpace(tmpl.GetDefaultTag())

	if expr := strings.TrimSpace(tmpl.GetTagTemplate()); expr != "" && engineVersion != "" {
		rendered, err := renderTag(expr, ParseVersion(engineVersion))
		if err != nil {
			return "", err
		}
		if rendered != "" {
			tag = rendered
		}
	}

	if tag == "" {
		return "", fmt.Errorf("%w: neither tag_template nor default_tag produced a tag for version %q",
			ErrInvalidTemplate, engineVersion)
	}
	if !tagPattern.MatchString(tag) {
		return "", fmt.Errorf("%w: %q is not a valid image tag", ErrInvalidTemplate, tag)
	}
	return tag, nil
}

// renderTag evaluates a tag_template against a parsed version.
func renderTag(expr string, v Version) (string, error) {
	parsed, err := template.New("tag").Parse(expr)
	if err != nil {
		return "", fmt.Errorf("%w: parse tag_template %q: %w", ErrInvalidTemplate, expr, err)
	}

	var out strings.Builder
	if err := parsed.Execute(&out, v); err != nil {
		return "", fmt.Errorf("%w: render tag_template %q: %w", ErrInvalidTemplate, expr, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// ImageRef builds the full image reference to pull for a sandbox.
func ImageRef(tmpl *fwv1.SandboxTemplate, engineVersion string) (string, error) {
	if tmpl == nil {
		return "", ErrNoTemplate
	}

	repository := strings.TrimSpace(tmpl.GetImageRepository())
	if !repositoryPattern.MatchString(repository) {
		return "", fmt.Errorf("%w: %q is not a valid image repository", ErrInvalidTemplate, repository)
	}

	tag, err := ResolveTag(tmpl, engineVersion)
	if err != nil {
		return "", err
	}
	return repository + ":" + tag, nil
}
