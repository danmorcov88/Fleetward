package acts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// sqlServerImage is the monitored engine the live acts use, and the one thing here big enough to be
// worth pulling before the demo starts rather than in the middle of it.
const sqlServerImage = "mcr.microsoft.com/mssql/server:2022-latest"

// dockerGIDAdvice is the fix for the one misconfiguration that breaks the acts everything builds to.
const dockerGIDAdvice = `  echo "FLEETWARD_DOCKER_GID=$(stat -c '%g' /var/run/docker.sock)" >> .env`

// actStack is act 0: the stack, and the pre-flight that makes its failures legible.
//
// Every failure in this act is boring — Docker is not running, the socket group is wrong, an image
// is not pulled — and every one of them, left unchecked, surfaces forty seconds later as a stack
// trace or, worse, as act 3 failing with something that reads like a product defect. So they are
// checked first, and each one fails with a sentence naming what to change.
func actStack(ctx context.Context, cfg Config, n *Narrator) error {
	n.Act(0, "The stack",
		"One command, on a real stack. Nothing here is mocked, so everything here can fail — "+
			"and the failures worth pre-empting are the boring ones.")

	// composeErr is deliberately not returned here. `--wait` fails if *any* service is unhealthy,
	// including one the demo asserts nothing about, and the question that actually matters is
	// whether the control plane came up — which /readyz answers directly. So the compose output is
	// carried forward and only reported if readiness never arrives.
	var composeOut string
	if cfg.Compose {
		if err := preflight(ctx, cfg, n); err != nil {
			return err
		}
		var composeErr error
		if composeOut, composeErr = composeUp(ctx, cfg, n); composeErr != nil {
			n.Step("compose reported a problem starting the stack; asking the control plane directly")
		}
	} else {
		n.Step("using the stack already running at %s", cfg.ServerURL)
	}

	status, err := waitReady(ctx, cfg, n)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(composeOut))
	}

	rows := make([][]string, 0, len(status.Components))
	for _, c := range status.Components {
		note := c.Error
		if note == "" {
			note = fmt.Sprintf("%d ms", c.LatencyMS)
		}
		rows = append(rows, []string{c.Name, c.Status, note})
	}
	n.Table([]string{"COMPONENT", "STATE", "NOTE"}, rows)
	n.Say("")
	n.Say("Control plane, metadata store, object store, metrics store, identity provider, "+
		"the web UI at %s, and a SQL Server for it to watch.", webURL())

	// The sandbox provider is the one component the demo cannot proceed without, because acts 3 and
	// 4 are the point of the whole thing and both of them start a container.
	if err := requireSandbox(status); err != nil {
		return err
	}
	if cfg.Compose {
		reportServicesThatDidNotStart(ctx, cfg, n)
	}
	n.Beat()
	return nil
}

// reportServicesThatDidNotStart says, loudly, which containers are not running.
//
// The demo asserts nothing through a browser, so a web container that failed to start does not make
// any act untrue — but it does make one sentence untrue, and act 4's whole visual beat is the estate
// view going red on a screen somebody is looking at. Saying so is the honest version; carrying on
// silently would be the demo claiming something it cannot show.
func reportServicesThatDidNotStart(ctx context.Context, cfg Config, n *Narrator) {
	out, err := run(ctx, cfg.Root, "docker", "compose", "ps", "-a", "--format", "{{.Service}} {{.State}}")
	if err != nil {
		return
	}
	var stopped []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] != "running" {
			stopped = append(stopped, fields[0])
		}
	}
	if len(stopped) == 0 {
		return
	}

	n.Say("")
	n.Say("NOT RUNNING: %s.", strings.Join(stopped, ", "))
	for _, name := range stopped {
		if name == "web" {
			n.Say("The estate view is therefore not reachable, and act 4's screen going red is the "+
				"one beat this run cannot show. Everything the acts assert goes through the API and "+
				"is unaffected. `docker compose logs web` says why; on this machine it has been "+
				"Node failing to reserve memory inside the Docker VM. Nothing about %s changes.",
				"Fleetward")
		}
	}
}

// preflight refuses early, and with an instruction rather than a diagnosis.
func preflight(ctx context.Context, cfg Config, n *Narrator) error {
	n.Step("checking Docker")
	if out, err := run(ctx, cfg.Root, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker is not reachable, and the whole demo runs on it — start Docker "+
			"Desktop (or the daemon) and run this again: %s", strings.TrimSpace(out))
	}
	if _, err := run(ctx, cfg.Root, "docker", "compose", "version"); err != nil {
		return errors.New("`docker compose` is not available. It ships with Docker Desktop, and as " +
			"the docker-compose-plugin package on Linux")
	}

	// The socket is root:root inside a Docker Desktop VM and root:docker on a Linux host, and the
	// control plane runs unprivileged. Getting this wrong makes verification fail with a permission
	// error the operator only sees after act 3 has already started.
	if err := requireDockerGroup(cfg); err != nil {
		return err
	}

	if _, err := run(ctx, cfg.Root, "docker", "image", "inspect", sqlServerImage); err != nil {
		n.Step("pulling %s — 625 MB, once", sqlServerImage)
		if out, pullErr := run(ctx, cfg.Root, "docker", "pull", sqlServerImage); pullErr != nil {
			return fmt.Errorf("pull %s: %w\n%s", sqlServerImage, pullErr, strings.TrimSpace(out))
		}
	}
	n.Step("Docker is reachable and the SQL Server image is present")
	return nil
}

// requireDockerGroup checks the one setting whose absence breaks the act everything builds to.
//
// Only on Linux: inside a Docker Desktop VM the socket is root:root and the compose default of
// group 0 is already correct, which is why the macOS and Windows quickstarts need no .env at all.
func requireDockerGroup(cfg Config) error {
	if runtime.GOOS != "linux" || os.Getenv("FLEETWARD_DOCKER_GID") != "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(cfg.Root, ".env"))
	if err == nil && strings.Contains(string(raw), "FLEETWARD_DOCKER_GID") {
		return nil
	}
	return errors.New("FLEETWARD_DOCKER_GID is not set, and on Linux the Docker socket is owned by " +
		"the docker group rather than by root.\n" +
		"The control plane runs unprivileged and provisions every verification sandbox through that " +
		"socket, so without this the demo reaches act 3 and cannot start a container:\n\n" +
		dockerGIDAdvice)
}

// composeUp brings the stack up. It returns compose's output alongside its error, because the
// caller decides whether a service that did not start matters.
//
// Deliberately without `--wait`. Compose's readiness is the union of every service's health check,
// including services the demo asserts nothing about, and it is answered by waiting for the whole
// timeout when one of them will never be healthy — seven minutes of a five-minute demo, spent on
// the web container. What the demo actually needs is answered better by the two checks it makes
// next: `/readyz`, which reports every dependency of the control plane by name, and a real query
// against the monitored database. Those poll, so they return the moment the answer is yes.
func composeUp(ctx context.Context, cfg Config, n *Narrator) (string, error) {
	args := []string{"compose", "up", "-d"}
	if cfg.Build {
		args = append(args, "--build")
	}
	n.Step("docker %s", strings.Join(args, " "))
	out, err := run(ctx, cfg.Root, "docker", args...)
	if err != nil {
		return out, fmt.Errorf("bringing the stack up: %w", err)
	}
	return out, nil
}

// ComposeDown is the teardown. Exported because the entry points own the decision to call it: a
// recording is followed by somebody wanting to click around, and CI is not.
func ComposeDown(ctx context.Context, cfg Config, n *Narrator) {
	if cfg.Keep {
		n.Say("The stack is still up: %s. `docker compose down --volumes` when you are done.", webURL())
		return
	}
	if !cfg.Compose {
		return
	}
	n.Step("docker compose down --volumes")
	if out, err := run(ctx, cfg.Root, "docker", "compose", "down", "--volumes", "--remove-orphans"); err != nil {
		n.Step("teardown reported a problem, which is not a demo failure: %v\n%s", err, strings.TrimSpace(out))
	}
}

// readyStatus mirrors the /readyz body.
type readyStatus struct {
	Status     string `json:"status"`
	Components []struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Critical  bool   `json:"critical"`
		Error     string `json:"error,omitempty"`
		LatencyMS int64  `json:"latency_ms"`
	} `json:"components"`
}

// waitReady polls /readyz until it is healthy, which is the foundation slice's own exit criterion.
func waitReady(ctx context.Context, cfg Config, n *Narrator) (readyStatus, error) {
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for {
		status, err := readyz(ctx, cfg)
		switch {
		case err != nil:
			last = err.Error()
		case status.Status == "healthy":
			n.Step("/readyz is healthy")
			return status, nil
		default:
			last = "/readyz reports " + status.Status
		}
		if time.Now().After(deadline) {
			return readyStatus{}, fmt.Errorf("the control plane never became ready: %s", last)
		}
		select {
		case <-ctx.Done():
			return readyStatus{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func readyz(ctx context.Context, cfg Config) (readyStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ServerURL+"/readyz", nil)
	if err != nil {
		return readyStatus{}, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return readyStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var status readyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return readyStatus{}, fmt.Errorf("decode /readyz: %w", err)
	}
	return status, nil
}

// requireSandbox fails act 0 rather than act 3 when verification cannot possibly work.
func requireSandbox(status readyStatus) error {
	for _, c := range status.Components {
		if c.Name != "sandbox" {
			continue
		}
		if c.Status == "healthy" {
			return nil
		}
		return fmt.Errorf("the sandbox provider is %s: %s\n"+
			"Verification restores every backup into a throwaway container, so acts 3 and 4 — the "+
			"whole point of this demo — cannot run without it. On Linux this is almost always the "+
			"socket group:\n\n%s", c.Status, c.Error, dockerGIDAdvice)
	}
	return errors.New("/readyz does not report a sandbox component, so verification cannot run")
}

// run executes a command in the repository root and returns its combined output.
func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func webURL() string { return "http://localhost:" + envOr("FLEETWARD_WEB_PORT", "3000") }
