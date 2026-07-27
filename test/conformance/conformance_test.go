//go:build conformance

// Package conformance holds the shared plugin conformance suite (ADR-0012).
//
// One suite runs against every plugin. It reads each plugin's capability matrix and exercises
// exactly what that plugin claims to support, skipping the rest — so it is useful from a plugin's
// first commit, and it grows in coverage automatically as capabilities are turned on.
//
// Passing this suite is the merge gate for any plugin change. It is also what lets a reviewer
// trust a plugin for an engine they have never operated: "does it pass conformance?" replaces
// reading a thousand lines of engine-specific code and hoping.
//
// Scope by stage:
//
//   - Stage 0 (now): capabilities and health. Every plugin binary must launch, handshake, declare
//     a coherent capability matrix, and refuse unimplemented RPCs cleanly.
//   - Stage 1: the full contract against a real engine via testcontainers — discover → metrics →
//     backup → restore-to-sandbox → verify → principals.
//
// Run with: make conformance
package conformance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/plugin/manager"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

// pluginDir is where `make build-plugins` puts the binaries.
const pluginDir = "../../bin/plugins"

// harness owns a manager with every built plugin loaded.
type harness struct {
	mgr     *manager.Manager
	engines []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir, err := filepath.Abs(pluginDir)
	if err != nil {
		t.Fatalf("resolve plugin dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("plugin directory %s not found; run `make build-plugins` first", dir)
	}

	mgr := manager.New(config.PluginsConfig{
		Dir:               dir,
		HandshakeTimeout:  60 * time.Second,
		RestartBackoffMin: time.Second,
		RestartBackoffMax: 10 * time.Second,
		HealthInterval:    30 * time.Second,
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("start plugin manager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	engines := mgr.EngineTypes()
	if len(engines) == 0 {
		t.Fatalf("no plugins found in %s; run `make build-plugins` first", dir)
	}

	return &harness{mgr: mgr, engines: engines}
}

// forEachPlugin runs fn as a subtest per plugin. Every conformance test uses it, so adding an
// engine automatically adds it to every existing assertion.
func (h *harness) forEachPlugin(t *testing.T, fn func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities)) {
	t.Helper()
	for _, engineType := range h.engines {
		t.Run(engineType, func(t *testing.T) {
			client, caps, err := h.mgr.Client(engineType)
			if err != nil {
				t.Fatalf("plugin %s is not ready: %v", engineType, err)
			}
			fn(t, engineType, client, caps)
		})
	}
}

// TestPluginLaunches asserts every plugin binary starts, handshakes, and reaches ready state.
func TestPluginLaunches(t *testing.T) {
	h := newHarness(t)

	for _, info := range h.mgr.List() {
		t.Run(info.EngineType, func(t *testing.T) {
			if info.State != manager.StateReady {
				t.Fatalf("state = %s, want ready: %s", info.State, info.Message)
			}
			if len(info.MissingTools) > 0 {
				// Not fatal: the tools may legitimately be absent on a CI runner. It is reported
				// because a plugin whose tooling is missing will fail its first real backup.
				t.Logf("missing declared tools: %v", info.MissingTools)
			}
		})
	}
}

// TestCapabilitiesAreCoherent asserts each plugin's matrix is internally consistent and correctly
// identifies itself. Core trusts this matrix when deciding what to do to a production database.
func TestCapabilitiesAreCoherent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		caps, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}

		if err := sdk.ValidateCapabilities(caps); err != nil {
			t.Errorf("capabilities are not coherent: %v", err)
		}
		if caps.GetEngineType() != engineType {
			t.Errorf("engine_type = %q, want %q (must match the binary name)", caps.GetEngineType(), engineType)
		}
		if caps.GetContractVersion() != version.ContractVersion {
			t.Errorf("contract_version = %q, want %q", caps.GetContractVersion(), version.ContractVersion)
		}
		if caps.GetEngineDisplayName() == "" {
			t.Error("engine_display_name is empty; the UI has nothing to show")
		}
	})
}

// TestGetCapabilitiesNeedsNoConnection asserts the capability matrix is available without a
// database. Core calls it at plugin startup, before any instance has been configured.
func TestGetCapabilitiesNeedsNoConnection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		// The request carries no ConnectionRef and no Credentials by construction.
		if _, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{}); err != nil {
			t.Fatalf("GetCapabilities must not require a connection: %v", err)
		}
	})
}

// TestCapabilitiesAreStable asserts repeated calls agree. Core caches the matrix for the plugin's
// process lifetime, so a plugin that varies its answer would leave core acting on a stale promise.
func TestCapabilitiesAreStable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		first, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}
		second, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities (second call): %v", err)
		}

		if first.GetEngineType() != second.GetEngineType() ||
			first.GetSupportsPitr() != second.GetSupportsPitr() ||
			first.GetSupportsSandboxRestore() != second.GetSupportsSandboxRestore() ||
			len(first.GetBackupMethods()) != len(second.GetBackupMethods()) {
			t.Error("capabilities changed between calls; core caches them for the process lifetime")
		}
	})
}

// TestUnsupportedRPCsAreRefusedCleanly asserts that an RPC a plugin has not implemented returns a
// typed refusal rather than hanging, panicking, or returning a nil response with a nil error.
//
// Core relies on this distinction to tell "this engine cannot do PITR" apart from "PITR is broken",
// and the UI shows the operator very different things for each.
func TestUnsupportedRPCsAreRefusedCleanly(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if len(caps.GetBackupMethods()) > 0 {
			t.Skip("plugin declares backup methods; the full contract is exercised by the Stage 1 suite")
		}

		// A plugin with no backup methods must refuse Backup rather than accept it.
		stream, err := client.Backup(ctx, &fwv1.BackupRequest{BackupId: "conformance-probe"})
		if err != nil {
			assertTypedRefusal(t, err)
			return
		}
		if _, err = stream.Recv(); err == nil {
			t.Fatal("a plugin with no backup methods accepted a Backup request")
		}
		assertTypedRefusal(t, err)
	})
}

// TestPITRWindowIsAnAnswerNotAnError asserts that a plugin without point-in-time recovery reports
// an unavailable window with a reason, rather than failing the call.
//
// The UI can explain "WAL archiving is disabled"; it cannot do anything useful with a stack trace.
func TestPITRWindowIsAnAnswerNotAnError(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		if caps.GetSupportsPitr() {
			t.Skip("plugin supports PITR; covered by the Stage 1 suite against a real instance")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		window, err := client.ListPITRTargets(ctx, &fwv1.ListPITRTargetsRequest{})
		if err != nil {
			t.Fatalf("ListPITRTargets must answer rather than fail when PITR is unsupported: %v", err)
		}
		if window.GetAvailable() {
			t.Error("window reports available = true but capabilities say supports_pitr = false")
		}
		if window.GetUnavailableReason() == "" {
			t.Error("unavailable_reason is empty; the UI has nothing to explain to the operator")
		}
	})
}

// assertTypedRefusal checks that an error carries a structured PluginError with a code core can act
// on, rather than an opaque string it would have to parse.
func assertTypedRefusal(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plugin hung instead of refusing: %v", err)
	}

	pe, ok := sdk.PluginErrorFrom(err)
	if !ok {
		t.Errorf("error carries no structured PluginError, so core cannot classify it: %v", err)
		return
	}
	if pe.GetCode() == fwv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("PluginError has no code: %v", pe.GetMessage())
	}
}
