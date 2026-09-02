package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// setting is one environment variable, joined from the two places the code states it: the struct
// field that carries its documentation, and the Load() call that names it and gives its default.
type setting struct {
	env       string
	section   string
	field     string
	doc       string
	def       string
	required  bool
	prodOnly  bool
	validated bool
}

// envReaders are the helpers Load() uses. Each takes the variable name and a default.
var envReaders = map[string]bool{
	"env": true, "envBool": true, "envInt": true, "envFloat": true,
	"envDuration": true, "envList": true,
}

// sections groups the reference the way an operator reads it, rather than the way the struct is
// declared. The order is roughly "what you must set" to "what you probably will not".
var sections = []struct {
	prefix string
	title  string
	note   string
}{
	{"", "Environment", ""},
	{"LOG_", "Logging", ""},
	{"METADB_", "Metadata store", "The only truly critical dependency: the control plane will not start without it."},
	{"HTTP_", "HTTP server and TLS", "TLS is off by default. Setting `HTTP_TLS_CLIENT_CA_FILE` additionally requires a client certificate."},
	{"GRPC_", "gRPC server", "Parsed and validated, and read by nothing: there is deliberately no gRPC listener ([ADR-0019](../adr/0019-rest-api-without-a-grpc-listener.md))."},
	{"OBJSTORE_", "Object storage for artifacts", ""},
	{"SECRETS_", "Secrets", "The security of every stored credential reduces to the protection of the master key."},
	{"SANDBOX_", "Verification sandboxes", "The `kubernetes` provider is not implemented; `docker` is the only working value."},
	{"PLUGINS_", "Engine plugins", ""},
	{"TSDB_", "Metrics store", "Constructed and health-checked. Nothing writes to it yet."},
	{"SCHEDULER_", "Scheduler", "Drives every run that nobody asked for. How these interact — the poll, the lease, the reaper — is [`scheduling.md`](scheduling.md)."},
	{"AUTH_", "Authentication", "Parsed and validated, and read by nothing. **There is no authentication yet, and every API route is open.**"},
	{"TELEMETRY_", "Telemetry", ""},
}

func genConfig(root string) (string, error) {
	src := filepath.Join(root, "internal", "config", "config.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("parsing config.go: %w", err)
	}

	consts = stringConsts(file)
	docs := fieldDocs(file)
	settings := loadCalls(file, docs)
	if len(settings) == 0 {
		return "", fmt.Errorf("found no environment variables — has Load() changed shape?")
	}
	markValidated(file, settings)

	var b strings.Builder
	b.WriteString(banner("internal/config/config.go"))
	b.WriteString(`# Configuration reference

Every setting the control plane reads, with its default. All of them are environment variables
prefixed ` + "`FLEETWARD_`" + `; the names below are written without the prefix, so ` + "`HTTP_ADDR`" + ` is set as
` + "`FLEETWARD_HTTP_ADDR`" + `.

Two parsing rules are worth knowing before reading the table. **An empty value counts as unset**, so
exporting a variable to the empty string restores its default rather than clearing it. And a
*malformed* number, duration, or boolean falls back to its default rather than failing to start —
which means a typo in a duration is silent, and the value you get is the one in this table.

`)

	used := map[string]bool{}
	for _, s := range sections {
		var rows [][]string
		for _, st := range settings {
			if used[st.env] || !inSection(st.env, s.prefix) {
				continue
			}
			used[st.env] = true
			rows = append(rows, []string{"`" + st.env + "`", st.def, note(st), st.doc})
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", s.title)
		if s.note != "" {
			b.WriteString(s.note + "\n\n")
		}
		b.WriteString(table([]string{"Variable", "Default", "", "Notes"}, rows))
		b.WriteString("\n")
	}

	var leftover [][]string
	for _, st := range settings {
		if !used[st.env] {
			leftover = append(leftover, []string{"`" + st.env + "`", st.def, note(st), st.doc})
		}
	}
	if len(leftover) > 0 {
		b.WriteString("## Other\n\n")
		b.WriteString(table([]string{"Variable", "Default", "", "Notes"}, leftover))
		b.WriteString("\n")
	}

	b.WriteString(`## Required

` + "`METADB_DSN`" + ` must be non-empty, and a master key must be supplied when the secrets provider is
` + "`aesgcm`" + ` — either ` + "`SECRETS_MASTER_KEY`" + ` or ` + "`SECRETS_MASTER_KEY_FILE`" + `. Enabling TLS on a listener
requires its certificate and key. Enabling authentication requires an issuer URL.

With ` + "`ENV=production`" + `, authentication may not be disabled and issuer verification may not be
skipped. Note what production mode does **not** require: TLS on any listener, TLS to the metadata
store or the object store, or a master key held in a file rather than an environment variable. A
production configuration therefore passes validation while serving plain HTTP — and while demanding
an authentication layer that does not exist yet.
`)
	return b.String(), nil
}

func inSection(env, prefix string) bool {
	if prefix == "" {
		// The unprefixed section takes only names with no underscore-delimited prefix of their own.
		return !strings.Contains(env, "_") || env == "ENV"
	}
	return strings.HasPrefix(env, prefix)
}

func note(s setting) string {
	switch {
	case s.required:
		return "**required**"
	case s.prodOnly:
		return "constrained in production"
	case s.validated:
		return "validated"
	}
	return ""
}

// fieldDocs collects the doc comment of every struct field, keyed "Type.Field".
func fieldDocs(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			text := commentText(f.Doc)
			if text == "" {
				text = commentText(f.Comment)
			}
			for _, name := range f.Names {
				out[ts.Name.Name+"."+name.Name] = text
			}
		}
		return true
	})
	return out
}

func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	// The comments wrap across lines, so cutting at the first newline truncated them mid-sentence —
	// and the second sentence of these comments is usually the one carrying the warning. Join the
	// whole thing onto one line; a table cell renders it fine.
	text := strings.Join(strings.Fields(strings.TrimSpace(g.Text())), " ")
	return strings.ReplaceAll(text, "|", "\\|")
}

// loadCalls walks every composite literal in the file looking for `Field: env("NAME", default)`,
// which is the one shape Load() uses.
func loadCalls(file *ast.File, docs map[string]string) []setting {
	var out []setting
	seen := map[string]bool{}

	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeName := ""
		if id, ok := lit.Type.(*ast.Ident); ok {
			typeName = id.Name
		}

		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			call := envCall(kv.Value)
			if call == nil {
				continue
			}
			name, ok := stringLit(call.Args[0])
			if !ok || seen[name] {
				continue
			}
			seen[name] = true

			field := ""
			if id, ok := kv.Key.(*ast.Ident); ok {
				field = id.Name
			}
			out = append(out, setting{
				env:     name,
				section: typeName,
				field:   field,
				doc:     docs[typeName+"."+field],
				def:     renderDefault(call.Args[1]),
			})
		}
		return true
	})

	sort.Slice(out, func(i, j int) bool { return out[i].env < out[j].env })
	return out
}

// envCall unwraps `env("NAME", default)`, including through a type conversion such as
// `Environment(env("ENV", ...))`, and returns nil for anything else.
func envCall(e ast.Expr) *ast.CallExpr {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return nil
	}
	if fn, ok := call.Fun.(*ast.Ident); ok && envReaders[fn.Name] && len(call.Args) == 2 {
		return call
	}
	if len(call.Args) == 1 {
		return envCall(call.Args[0])
	}
	return nil
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// consts holds the file's string constants, so a default written as `string(EnvDevelopment)` can be
// reported as the value an operator would actually type.
var consts map[string]string

func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if i < len(vs.Values) {
				if s, ok := stringLit(vs.Values[i]); ok {
					out[name.Name] = s
				}
			}
		}
		return true
	})
	return out
}

// renderDefault turns the default argument back into something an operator can type.
func renderDefault(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLit(v); ok {
			if s == "" {
				return "*(empty)*"
			}
			return "`" + s + "`"
		}
		return "`" + v.Value + "`"
	case *ast.CallExpr:
		// A type conversion around the real default, such as string(EnvDevelopment).
		if len(v.Args) == 1 {
			return renderDefault(v.Args[0])
		}
	case *ast.Ident:
		if s, ok := consts[v.Name]; ok {
			return "`" + s + "`"
		}
		return "`" + v.Name + "`"
	case *ast.SelectorExpr:
		// time.Second, time.Minute
		return "`" + durationName(v) + "`"
	case *ast.BinaryExpr:
		// 30*time.Second
		left := strings.Trim(renderDefault(v.X), "`")
		right := strings.Trim(renderDefault(v.Y), "`")
		return "`" + humanDuration(left, right) + "`"
	case *ast.CompositeLit:
		var parts []string
		for _, el := range v.Elts {
			if s, ok := stringLit(el); ok {
				parts = append(parts, s)
			}
		}
		return "`" + strings.Join(parts, ",") + "`"
	}
	return ""
}

func durationName(s *ast.SelectorExpr) string {
	switch s.Sel.Name {
	case "Second":
		return "1s"
	case "Minute":
		return "1m"
	case "Hour":
		return "1h"
	}
	return s.Sel.Name
}

// humanDuration turns "30" and "1s" into "30s", which is what the operator would type.
func humanDuration(count, unit string) string {
	if len(unit) == 2 && unit[0] == '1' {
		return count + string(unit[1])
	}
	return count + "*" + unit
}

// markValidated finds the settings Validate() insists on, so the reference can say which are
// required rather than leaving an operator to discover it at startup.
func markValidated(file *ast.File, settings []setting) {
	byName := map[string]*setting{}
	for i := range settings {
		byName[settings[i].env] = &settings[i]
	}

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Validate" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			for name, s := range byName {
				if !strings.Contains(text, name+":") {
					continue
				}
				switch {
				case strings.Contains(text, "required"):
					s.required = true
				case strings.Contains(text, "production"):
					s.prodOnly = true
				default:
					s.validated = true
				}
			}
			return true
		})
		return false
	})
}
