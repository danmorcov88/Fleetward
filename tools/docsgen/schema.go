package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// column is one column of one table, reduced to what an entity diagram shows.
type column struct {
	name       string
	typ        string
	primaryKey bool
	references string // the table it points at, empty when it is not a foreign key
}

type schemaTable struct {
	name    string
	columns []column
}

var (
	createTable = regexp.MustCompile(`(?i)^CREATE TABLE (\w+) \($`)
	columnLine  = regexp.MustCompile(`^\s+(\w+)\s+([A-Za-z]+(?:\([^)]*\))?)`)
	referencesT = regexp.MustCompile(`(?i)REFERENCES\s+(\w+)`)
)

// groups decide which tables are drawn together. Eighteen tables in one entity diagram is a
// hairball nobody reads, and the grouping is editorial rather than derivable — a table belongs with
// the question it answers, not with whatever it happens to reference.
//
// A table missing from every group is reported rather than silently dropped, so a migration that
// adds one cannot quietly leave it undrawn.
var groups = []struct {
	title  string
	blurb  string
	tables []string
}{
	{
		title: "Identity and tenancy",
		blurb: "Every tenant-scoped table carries `tenant_id` from the first migration. Retrofitting " +
			"tenancy would mean a full-schema migration plus an audit of every query, so the column " +
			"is carried unused rather than added later ([ADR-0008](../adr/0008-oidc-rbac-multitenancy.md)).",
		tables: []string{"tenants", "users", "roles", "role_grants"},
	},
	{
		title: "Inventory",
		blurb: "What is being watched, and how to reach it. Credentials are split: everything " +
			"non-secret is on `connections`, while the password and any client private key go to " +
			"the secrets provider under a name that row carries.",
		tables: []string{"environments", "instances", "connections", "secrets"},
	},
	{
		title: "Operations",
		blurb: "Scheduled work and its results. `jobs` carries the lease columns an at-most-once " +
			"scheduler needs — `lease_owner`, `lease_expires_at`, `heartbeat_at` — which nothing " +
			"writes yet.",
		tables: []string{"schedules", "jobs", "backups", "verifications", "restores"},
	},
	{
		title: "Observability",
		blurb: "Facts, conditions, and the record of who did what. `events` are facts; `alerts` are " +
			"conditions with a lifecycle. `audit_log` has a trigger that rejects UPDATE and DELETE, " +
			"because an audit log that can be edited is not evidence.",
		tables: []string{"alert_rules", "alerts", "notifiers", "events", "audit_log"},
	},
}

func genSchema(root string) (string, error) {
	src := filepath.Join(root, "internal", "storage", "metadb", "migrations", "000001_init.up.sql")
	b, err := os.ReadFile(src) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		return "", fmt.Errorf("reading the migration: %w", err)
	}

	tables := parseSchema(string(b))
	if len(tables) == 0 {
		return "", fmt.Errorf("found no tables — has the migration changed shape?")
	}

	byName := map[string]schemaTable{}
	for _, t := range tables {
		byName[t.name] = t
	}

	grouped := map[string]bool{}
	for _, g := range groups {
		for _, name := range g.tables {
			if _, ok := byName[name]; !ok {
				return "", fmt.Errorf("group %q lists %q, which the migration does not define", g.title, name)
			}
			grouped[name] = true
		}
	}
	var ungrouped []string
	for _, t := range tables {
		if !grouped[t.name] {
			ungrouped = append(ungrouped, t.name)
		}
	}
	if len(ungrouped) > 0 {
		sort.Strings(ungrouped)
		return "", fmt.Errorf("the migration defines %s, which no group in tools/docsgen/schema.go "+
			"draws — add them to a group so they cannot go undrawn", strings.Join(ungrouped, ", "))
	}

	var out strings.Builder
	out.WriteString(banner("internal/storage/metadb/migrations/000001_init.up.sql"))
	fmt.Fprintf(&out, `# Data model

%d tables, grouped by the question they answer. The schema is deliberately ahead of the code: the
scheduler, alerting, and RBAC tables are complete and unused, because a column added later costs a
migration and an audit of every query, while a column carried unused costs nothing.

Drawn in four diagrams rather than one. All %d tables in a single entity diagram is a picture nobody
reads.

`, len(tables), len(tables))

	for _, g := range groups {
		fmt.Fprintf(&out, "## %s\n\n%s\n\n", g.title, g.blurb)
		out.WriteString(erDiagram(g.tables, byName))
		out.WriteString("\n")
	}

	out.WriteString("## Every table\n\n")
	var rows [][]string
	for _, t := range tables {
		var fks []string
		for _, c := range t.columns {
			if c.references != "" {
				fks = append(fks, c.references)
			}
		}
		tenant := ""
		for _, c := range t.columns {
			if c.name == "tenant_id" {
				tenant = "yes"
			}
		}
		rows = append(rows, []string{
			"`" + t.name + "`",
			fmt.Sprint(len(t.columns)),
			tenant,
			strings.Join(uniq(fks), ", "),
		})
	}
	out.WriteString(table([]string{"Table", "Columns", "Tenant-scoped", "References"}, rows))

	return out.String(), nil
}

// erDiagram renders one mermaid entity diagram: the relationships first, then each table's key
// columns. Only keys are drawn — a diagram listing every column of `backups` would be a schema
// dump, and the table below already carries the counts.
func erDiagram(names []string, byName map[string]schemaTable) string {
	var b strings.Builder
	b.WriteString("```mermaid\nerDiagram\n")

	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}

	for _, n := range names {
		t := byName[n]
		for _, c := range t.columns {
			// Relationships to tables outside this group are left out: an edge to a box that is not
			// drawn is noise, and the "Every table" listing carries them.
			if c.references == "" || !in[c.references] || c.references == n {
				continue
			}
			fmt.Fprintf(&b, "    %s ||--o{ %s : %s\n", c.references, n, c.name)
		}
	}

	for _, n := range names {
		t := byName[n]
		fmt.Fprintf(&b, "    %s {\n", t.name)
		for _, c := range t.columns {
			switch {
			case c.primaryKey:
				fmt.Fprintf(&b, "        %s %s PK\n", c.typ, c.name)
			case c.references != "":
				fmt.Fprintf(&b, "        %s %s FK\n", c.typ, c.name)
			}
		}
		b.WriteString("    }\n")
	}

	b.WriteString("```\n")
	return b.String()
}

func parseSchema(sql string) []schemaTable {
	var out []schemaTable
	var current *schemaTable

	for _, raw := range strings.Split(strings.ReplaceAll(sql, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		if m := createTable.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, schemaTable{name: m[1]})
			current = &out[len(out)-1]
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), ")") {
			current = nil
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		// Table-level constraints are not columns.
		upper := strings.ToUpper(trimmed)
		for _, kw := range []string{"UNIQUE", "CHECK", "PRIMARY KEY", "FOREIGN KEY", "CONSTRAINT", "EXCLUDE"} {
			if strings.HasPrefix(upper, kw) {
				trimmed = ""
				break
			}
		}
		if trimmed == "" {
			continue
		}

		m := columnLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		c := column{
			name:       m[1],
			typ:        strings.ToLower(m[2]),
			primaryKey: strings.Contains(upper, "PRIMARY KEY"),
		}
		if r := referencesT.FindStringSubmatch(trimmed); r != nil {
			c.references = r[1]
		}
		current.columns = append(current.columns, c)
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
