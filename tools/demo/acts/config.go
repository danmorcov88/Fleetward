// Package acts is the Fleetward demo: the product's story told against a real stack, as ordinary
// Go functions that take a client and a narrator.
//
// It has two entry points and one implementation, which is the whole idea (ADR-0037):
//
//   - `go run ./tools/demo` presents it, with narration and a pause between acts.
//   - `go test -tags=e2e ./test/e2e/...` runs the same acts in CI, quietly, asserting every one.
//
// So the demo cannot drift from the product without a CI job going red, and the end-to-end test
// reads as the workflow a user actually performs rather than as a list of API calls.
//
// Four rules hold over everything in this package, and none of them is negotiable:
//
//  1. The demo shows nothing the product does not do. No mocked screens, no staged alerts.
//  2. Seeded history is labelled seeded, on screen while it runs and in docs/demo.md.
//  3. The live acts are live: real containers, real artifacts, real bytes corrupted in a real
//     object store.
//  4. CI runs it, which is what keeps the first three true.
//
// Nothing here imports `internal/`. The demo drives the REST API and the metadata database exactly
// as any other client would; the moment it reached into the code it would stop being a demo of the
// product and become a demo of the code.
package acts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is everything the acts need to find the stack. The defaults are the development stack's
// published values, which are in docker-compose.yml and in .env.example already.
type Config struct {
	// ServerURL is the control plane's REST base URL.
	ServerURL string
	// Token is the bootstrap credential the seeder authenticates with. It is tenant-wide
	// administrator, it is a known value in a public repository, and docs/ops/authorization.md
	// already says what that means.
	Token string

	// MetaDSN reaches the metadata database directly, and exactly one thing uses it: seeding
	// backups that happened in the past. There is no API for that and there should not be — you
	// cannot ask Fleetward to have taken a backup last Tuesday.
	MetaDSN string

	// Object store, for one act: overwriting an artifact's bytes where they actually live.
	ObjectEndpoint  string
	ObjectAccessKey string
	ObjectSecretKey string
	ObjectUseSSL    bool

	// Compose brings the stack up in act 0 and takes it down in the teardown.
	Compose bool
	// Keep leaves the stack running afterwards, because after a recording somebody always wants to
	// click around.
	Keep bool
	// Build passes --build to `docker compose up`. Off by default locally, on in CI.
	Build bool

	// Theatre turns on the presentation: pauses between acts, and the sentences that are there for
	// a person watching rather than for a log.
	Theatre bool
	// Pause is how long the narrator waits between acts when Theatre is on.
	Pause time.Duration

	// Out receives everything the narrator writes. A transcript is this writer teed to a file.
	Out io.Writer

	// Root is the repository root, where docker-compose.yml lives.
	Root string
}

// Environment variable names, all of which already exist for the development stack.
const (
	envServer     = "FLEETWARD_SERVER"
	envToken      = "FLEETWARD_TOKEN"
	envMetaDSN    = "FLEETWARD_DEMO_METADB_DSN"
	envObjectHost = "FLEETWARD_DEMO_OBJSTORE_ENDPOINT"
	envPostgres   = "FLEETWARD_POSTGRES_PORT"
	envMinIO      = "FLEETWARD_MINIO_PORT"
	envHTTP       = "FLEETWARD_HTTP_PORT"
)

// DefaultConfig is the development stack as docker-compose.yml publishes it, with the same host
// port overrides .env.example documents applied.
func DefaultConfig() (Config, error) {
	root, err := repoRoot()
	if err != nil {
		return Config{}, err
	}
	// The demo has to read `.env` because compose does. A machine that already runs a PostgreSQL
	// puts FLEETWARD_POSTGRES_PORT there, compose publishes on that port, and a demo that only
	// consulted its own process environment would then go looking for the stack's metadata database
	// on 5432 — where it would find the machine's own.
	dotenv = readDotenv(filepath.Join(root, ".env"))

	// Every literal below is published in docker-compose.yml and in .env.example already: this is a
	// development stack somebody started on their own machine, and docs/ops/authorization.md says
	// what that means.
	// 127.0.0.1 rather than localhost, throughout, and it is not cosmetic. localhost resolves to
	// both ::1 and 127.0.0.1, and a developer machine with its own PostgreSQL listening on ::1:5432
	// answers the demo's metadata connection with a password failure that reads like a broken
	// stack. Compose's own health checks say 127.0.0.1 for the same reason.
	cfg := Config{ //nolint:gosec // G101: the development stack's own published credentials
		ServerURL: envOr(envServer, "http://127.0.0.1:"+envOr(envHTTP, "8080")),
		// The bootstrap token from docker-compose.yml. Configuration, never a database row
		// (ADR-0033), and deliberately the credential a fresh `docker compose up` already has.
		Token: envOr(envToken, "fwt_devbootstrap_0000000000000000"),
		MetaDSN: envOr(envMetaDSN,
			"postgres://fleetward:fleetward@127.0.0.1:"+envOr(envPostgres, "5432")+"/fleetward?sslmode=disable"),
		ObjectEndpoint:  envOr(envObjectHost, "127.0.0.1:"+envOr(envMinIO, "9000")),
		ObjectAccessKey: "fleetward",
		ObjectSecretKey: "fleetward-dev-secret",
		Compose:         true,
		Pause:           2 * time.Second,
		Out:             os.Stdout,
		Root:            root,
	}
	return cfg, nil
}

// repoRoot walks up from the working directory looking for docker-compose.yml.
//
// It is found rather than assumed because the two entry points start in different places: `go run
// ./tools/demo` runs from wherever the operator typed it, and `go test ./test/e2e/...` runs from
// the test's own directory.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("locate the working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "docker-compose.yml")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(
				"no docker-compose.yml above the working directory: run the demo from inside the repository")
		}
		dir = parent
	}
}

// dotenv holds what `.env` set, read once by DefaultConfig. It is consulted below the process
// environment, which is the precedence compose itself uses.
var dotenv map[string]string

// readDotenv parses the subset of `.env` this repository's own file uses: `KEY=VALUE`, one per
// line, `#` comments, no quoting and no interpolation. Anything richer is a dependency, and
// `.env.example` shows exactly this shape.
func readDotenv(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the repository's own .env, beside compose
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if v := dotenv[name]; v != "" {
		return v
	}
	return fallback
}
