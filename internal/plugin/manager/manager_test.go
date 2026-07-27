package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/version"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	minDelay, maxDelay := time.Second, 30*time.Second

	var previous time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		got := backoff(minDelay, maxDelay, attempt)

		// Jitter is ±20%, so the bound below is the cap plus its own jitter allowance.
		if got < minDelay {
			t.Errorf("attempt %d: %s is below the minimum %s", attempt, got, minDelay)
		}
		if got > maxDelay+maxDelay/5 {
			t.Errorf("attempt %d: %s exceeds the cap %s plus jitter", attempt, got, maxDelay)
		}
		// Growth is checked loosely because jitter can make one attempt shorter than the last.
		if attempt > 1 && attempt <= 5 && got < previous/2 {
			t.Errorf("attempt %d: %s dropped far below the previous %s", attempt, got, previous)
		}
		previous = got
	}
}

func TestBackoffToleratesUnsetBounds(t *testing.T) {
	tests := []struct {
		name             string
		minimum, maximum time.Duration
	}{
		{"both zero", 0, 0},
		{"zero min", 0, time.Minute},
		{"zero max", time.Second, 0},
		{"max below min", 10 * time.Second, time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A misconfigured backoff must still produce a usable delay rather than busy-looping
			// on restart or panicking on a zero-length jitter range.
			if got := backoff(tc.minimum, tc.maximum, 3); got <= 0 {
				t.Fatalf("backoff = %s, want a positive duration", got)
			}
		})
	}
}

func TestDiscoverBinaries(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write(BinaryPrefix+"postgresql", 0o755)
	write(BinaryPrefix+"redis", 0o755)
	write("unrelated-binary", 0o755)
	write("README.md", 0o644)
	if err := os.Mkdir(filepath.Join(dir, BinaryPrefix+"adirectory"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	found, err := discoverBinaries(dir)
	if err != nil {
		t.Fatalf("discoverBinaries: %v", err)
	}

	if len(found) != 2 {
		t.Fatalf("found %d plugins, want 2: %v", len(found), found)
	}
	for _, engine := range []string{"postgresql", "redis"} {
		if _, ok := found[engine]; !ok {
			t.Errorf("engine %q not discovered", engine)
		}
	}
}

func TestDiscoverBinariesRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BinaryPrefix+"postgresql"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Silently skipping a non-executable plugin would present as "engine not supported", sending
	// an operator hunting in the wrong place.
	if _, err := discoverBinaries(dir); err == nil {
		t.Fatal("expected an error for a non-executable plugin binary")
	}
}

func TestDiscoverBinariesMissingDirIsNotAnError(t *testing.T) {
	found, err := discoverBinaries(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing plugin dir should not be an error: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d plugins in a missing directory", len(found))
	}
}

func TestValidateHandshake(t *testing.T) {
	valid := func() *fwv1.Capabilities {
		return &fwv1.Capabilities{
			EngineType:      "postgresql",
			PluginVersion:   "0.1.0",
			ContractVersion: version.ContractVersion,
		}
	}

	tests := []struct {
		name       string
		engineType string
		mutate     func(*fwv1.Capabilities)
		wantErr    bool
	}{
		{"valid", "postgresql", nil, false},
		{"engine type mismatch", "postgresql", func(c *fwv1.Capabilities) {
			c.EngineType = "mysql"
		}, true},
		{"contract version mismatch", "postgresql", func(c *fwv1.Capabilities) {
			c.ContractVersion = "v99"
		}, true},
		{"missing plugin version", "postgresql", func(c *fwv1.Capabilities) {
			c.PluginVersion = ""
		}, true},
		{"incoherent capabilities", "postgresql", func(c *fwv1.Capabilities) {
			c.SupportsPitr = true // but no backup method sets enables_pitr
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := valid()
			if tc.mutate != nil {
				tc.mutate(caps)
			}
			err := validateHandshake(tc.engineType, caps)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateHandshake() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestMissingTools(t *testing.T) {
	caps := &fwv1.Capabilities{
		// "go" is on PATH in any environment that can run this test; the other two are not real.
		RequiredTools: []string{"go", "fleetward-definitely-not-a-real-tool"},
		BackupMethods: []*fwv1.BackupMethod{
			{Id: "m1", RequiredTools: []string{"fleetward-also-not-real", "go"}},
		},
	}

	missing := missingTools(caps)
	want := []string{"fleetward-also-not-real", "fleetward-definitely-not-a-real-tool"}
	if len(missing) != len(want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("missing[%d] = %q, want %q", i, missing[i], want[i])
		}
	}
}

func TestManagerWithNoPlugins(t *testing.T) {
	m := New(config.PluginsConfig{Dir: filepath.Join(t.TempDir(), "absent")}, discardLogger())
	t.Cleanup(func() { _ = m.Close() })

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(m.List()); got != 0 {
		t.Fatalf("List returned %d plugins, want 0", got)
	}
	if _, _, err := m.Client("postgresql"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("Client error = %v, want ErrPluginNotFound", err)
	}
	// A control plane with no plugins is healthy; it simply cannot serve any engine yet.
	if err := m.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck with no plugins: %v", err)
	}
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	m := New(config.PluginsConfig{Dir: t.TempDir()}, discardLogger())
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := range 3 {
		if err := m.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}

// TestManagerLaunchesRealPlugin builds an actual plugin binary and drives it end to end through
// go-plugin.
//
// This is the test that proves the architecture works: process launch, mTLS handshake, capability
// exchange, contract-version check, and a real gRPC call across the boundary. Everything else in
// this package is bookkeeping around it.
func TestManagerLaunchesRealPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary; skipped in -short mode")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, BinaryPrefix+"postgresql")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary,
		"github.com/danmorcov88/fleetward/cmd/plugins/postgres")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	m := New(config.PluginsConfig{
		Dir:               dir,
		HandshakeTimeout:  30 * time.Second,
		RestartBackoffMin: 50 * time.Millisecond,
		RestartBackoffMax: time.Second,
		HealthInterval:    100 * time.Millisecond,
	}, discardLogger())
	t.Cleanup(func() { _ = m.Close() })

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	infos := m.List()
	if len(infos) != 1 {
		t.Fatalf("List returned %d plugins, want 1", len(infos))
	}
	if infos[0].State != StateReady {
		t.Fatalf("plugin state = %s (%s), want ready", infos[0].State, infos[0].Message)
	}

	client, caps, err := m.Client("postgresql")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if caps.GetEngineType() != "postgresql" {
		t.Errorf("engine_type = %q, want %q", caps.GetEngineType(), "postgresql")
	}
	if caps.GetContractVersion() != version.ContractVersion {
		t.Errorf("contract_version = %q, want %q", caps.GetContractVersion(), version.ContractVersion)
	}

	// A live RPC across the process boundary.
	got, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities over gRPC: %v", err)
	}
	if got.GetEngineDisplayName() != "PostgreSQL" {
		t.Errorf("engine_display_name = %q, want %q", got.GetEngineDisplayName(), "PostgreSQL")
	}

	// An unimplemented RPC must come back as a clean typed refusal, not as a crash or a hang.
	// Core relies on this to tell "this engine cannot do PITR" apart from "PITR is broken".
	if _, err := client.Discover(ctx, &fwv1.DiscoverRequest{}); err == nil {
		t.Fatal("expected Discover to be refused by a Stage 0 plugin")
	}
}

// TestManagerRestartsCrashedPlugin verifies the supervisor notices a dead process and brings it
// back. A plugin that dies mid-shift and stays dead would silently stop every backup for its
// engine.
func TestManagerRestartsCrashedPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary; skipped in -short mode")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, BinaryPrefix+"postgresql")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary,
		"github.com/danmorcov88/fleetward/cmd/plugins/postgres")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	m := New(config.PluginsConfig{
		Dir:               dir,
		HandshakeTimeout:  30 * time.Second,
		RestartBackoffMin: 10 * time.Millisecond,
		RestartBackoffMax: 100 * time.Millisecond,
		HealthInterval:    50 * time.Millisecond,
	}, discardLogger())
	t.Cleanup(func() { _ = m.Close() })

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	m.mu.RLock()
	p := m.plugins["postgresql"]
	m.mu.RUnlock()
	if p == nil {
		t.Fatal("plugin was not registered")
	}

	// Kill the process out from under the manager, the way a real crash would.
	p.mu.Lock()
	p.client.Kill()
	p.mu.Unlock()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		info := p.info()
		if info.State == StateReady && info.RestartCount > 0 {
			return // restarted
		}
		time.Sleep(50 * time.Millisecond)
	}

	info := p.info()
	t.Fatalf("plugin was not restarted: state=%s restarts=%d message=%s",
		info.State, info.RestartCount, info.Message)
}
