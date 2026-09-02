package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestParseSchema(t *testing.T) {
	sql := `
-- A comment before the table.
CREATE TABLE tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (slug)
);

CREATE TABLE instances (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- Canonical engine identifier. Core must never branch on this value.
    engine_type    TEXT        NOT NULL,
    port           INTEGER     NOT NULL CHECK (port > 0 AND port <= 65535),

    UNIQUE (tenant_id, name)
);
`

	tables := parseSchema(sql)
	if len(tables) != 2 {
		t.Fatalf("parsed %d tables, want 2: %+v", len(tables), tables)
	}

	if tables[0].name != "tenants" || len(tables[0].columns) != 3 {
		t.Errorf("tenants: got %s with %d columns, want 3", tables[0].name, len(tables[0].columns))
	}
	if !tables[0].columns[0].primaryKey {
		t.Error("tenants.id was not recognised as a primary key")
	}

	inst := tables[1]
	if len(inst.columns) != 4 {
		t.Fatalf("instances: got %d columns, want 4: %+v", len(inst.columns), inst.columns)
	}
	if inst.columns[1].references != "tenants" {
		t.Errorf("instances.tenant_id references %q, want tenants", inst.columns[1].references)
	}
	// A CHECK constraint on a column line must not be mistaken for a table-level constraint, and a
	// table-level UNIQUE must not be mistaken for a column.
	if inst.columns[3].name != "port" {
		t.Errorf("last column is %q, want port — a table-level constraint was probably parsed as one",
			inst.columns[3].name)
	}
}

// TestRenderDefault covers the shapes Load() actually uses. A default reported wrongly is worse
// than one omitted: an operator reads this table instead of the code, which is the entire point.
func TestRenderDefault(t *testing.T) {
	consts = map[string]string{"EnvDevelopment": "development"}

	tests := []struct {
		name string
		expr string
		want string
	}{
		{"string", `env("X", "hello")`, "`hello`"},
		{"empty string", `env("X", "")`, "*(empty)*"},
		{"bool", `envBool("X", false)`, "`false`"},
		{"int", `envInt("X", 20)`, "`20`"},
		{"duration unit", `envDuration("X", time.Second)`, "`1s`"},
		{"duration product", `envDuration("X", 30*time.Second)`, "`30s`"},
		{"duration minutes", `envDuration("X", 5*time.Minute)`, "`5m`"},
		{"type conversion around a constant", `env("X", string(EnvDevelopment))`, "`development`"},
		{"string list", `envList("X", []string{"openid", "email"})`, "`openid,email`"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.expr, err)
			}
			call := envCall(e)
			if call == nil {
				t.Fatalf("envCall did not recognise %q", tc.expr)
			}
			if got := renderDefault(call.Args[1]); got != tc.want {
				t.Errorf("renderDefault(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

// TestEnvCallUnwrapsConversions guards the case that was silently dropping a setting: the
// environment itself is read as Environment(env("ENV", ...)), so a matcher looking only for a bare
// env() call missed it entirely and the reference had no Environment section.
func TestEnvCallUnwrapsConversions(t *testing.T) {
	tests := []struct {
		expr string
		want bool
	}{
		{`env("ENV", "development")`, true},
		{`Environment(env("ENV", "development"))`, true},
		{`someOtherCall("ENV", "development")`, false},
		{`"just a string"`, false},
	}

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			if got := envCall(e) != nil; got != tc.want {
				t.Errorf("envCall(%q) recognised = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// TestCommentTextJoinsWrappedLines guards a truncation bug: taking the first line of a doc comment
// cut every wrapped sentence in half, and the second sentence of these comments is usually the one
// carrying the warning an operator needs.
func TestCommentTextJoinsWrappedLines(t *testing.T) {
	src := `package config

type C struct {
	// AutoMigrate runs pending migrations at startup. Convenient in development; in production
	// most operators prefer a deliberate, separate step.
	AutoMigrate bool
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := fieldDocs(file)["C.AutoMigrate"]
	if !strings.Contains(got, "deliberate, separate step") {
		t.Errorf("comment was truncated: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("comment still contains a newline, which breaks the table cell: %q", got)
	}
}

// TestTableLeavesTheLastColumnUnpadded checks the one formatting rule that matters for review:
// earlier columns are padded so the committed source lines up, and the last one — which holds prose
// — is not, because padding every row out to the widest sentence makes the diff unreadable.
func TestTableLeavesTheLastColumnUnpadded(t *testing.T) {
	got := table(
		[]string{"A", "B"},
		[][]string{{"x", "a long trailing cell"}, {"yy", "short"}},
	)

	lines := strings.Split(strings.TrimSpace(got), "\n")
	for i, line := range lines {
		if i == 1 {
			continue // the separator row is padded on purpose
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		last := cells[len(cells)-1]
		if last != " "+strings.TrimSpace(last)+" " {
			t.Errorf("line %d: last cell %q is padded", i, last)
		}
	}

	// And the first column is padded, so the table still lines up where it is cheap to do so.
	if !strings.HasPrefix(lines[0], "| A  |") {
		t.Errorf("first column is not padded; header row is %q", lines[0])
	}
}
