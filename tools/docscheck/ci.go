package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// checkCI asserts that three descriptions of the merge gate agree: the jobs the workflow defines,
// the checks the branch ruleset requires, and the number the documentation quotes.
//
// The middle one is the reason this check earns its place, and it is a security check rather than a
// tidiness one. A job that runs but is not required is a job whose failure does not block a merge —
// the conformance suite, or the Windows portability tests, could have been silently advisory for
// months and nothing would have said so. The prose count is the cheap third leg: the README diagram
// drew seven nodes while eight jobs ran, and the one it omitted was conformance.
func checkCI(r *repo) []finding {
	var out []finding

	jobs, err := workflowJobNames(r, ".github/workflows/ci.yml")
	if err != nil {
		return []finding{{file: ".github/workflows/ci.yml", line: 0, msg: err.Error()}}
	}
	required, err := rulesetContexts(r, ".github/rulesets/main-protection.json")
	if err != nil {
		return []finding{{file: ".github/rulesets/main-protection.json", line: 0, msg: err.Error()}}
	}

	for _, name := range difference(jobs, required) {
		out = append(out, finding{
			file: ".github/rulesets/main-protection.json", line: 0,
			msg:  fmt.Sprintf("CI runs %q but the ruleset does not require it", name),
			hint: "a job that cannot block a merge is not a merge gate",
		})
	}
	for _, name := range difference(required, jobs) {
		out = append(out, finding{
			file: ".github/workflows/ci.yml", line: 0,
			msg: fmt.Sprintf("the ruleset requires %q but no CI job produces it", name),
			hint: "every pull request will wait for a check that never arrives — this blocks all " +
				"merges until fixed",
		})
	}

	out = append(out, checkCountMarker(r, "ci-jobs", len(jobs))...)
	out = append(out, checkSpelledCount(r, jobs)...)
	return out
}

// ciWorkflow is the fragment of a GitHub Actions workflow this check needs.
type ciWorkflow struct {
	Jobs map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"jobs"`
}

func workflowJobNames(r *repo, path string) ([]string, error) {
	b, err := r.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the workflow: %w", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		return nil, fmt.Errorf("parsing the workflow: %w", err)
	}
	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("defines no jobs")
	}

	names := make([]string, 0, len(wf.Jobs))
	for id, job := range wf.Jobs {
		// A job with no display name reports its id as the check context.
		if job.Name != "" {
			names = append(names, job.Name)
			continue
		}
		names = append(names, id)
	}
	sort.Strings(names)
	return names, nil
}

// ruleset is the fragment of a GitHub repository ruleset this check needs.
type ruleset struct {
	Rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	} `json:"rules"`
}

func rulesetContexts(r *repo, path string) ([]string, error) {
	b, err := r.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the ruleset: %w", err)
	}
	var rs ruleset
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("parsing the ruleset: %w", err)
	}

	var names []string
	for _, rule := range rs.Rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, c := range rule.Parameters.RequiredStatusChecks {
			names = append(names, c.Context)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("requires no status checks")
	}
	sort.Strings(names)
	return names, nil
}

// spelled maps the number words the documentation uses to their values. Prose says "all nine CI
// jobs", not "all 9", so a numeric check would miss the sentence that actually goes stale.
var spelled = map[string]int{
	"four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
	"nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
}

func checkSpelledCount(r *repo, jobs []string) []finding {
	var out []finding
	for _, d := range r.docs {
		for i, line := range d.lines {
			lower := strings.ToLower(line)
			if !strings.Contains(lower, "ci job") {
				continue
			}
			for word, n := range spelled {
				if !strings.Contains(lower, word+" ci job") {
					continue
				}
				if n != len(jobs) {
					out = append(out, finding{
						file: d.path, line: i + 1,
						msg: fmt.Sprintf("says %q, but CI defines %d jobs", word+" CI jobs", len(jobs)),
					})
				}
			}
		}
	}
	return out
}

// difference returns the elements of a that are not in b.
func difference(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
