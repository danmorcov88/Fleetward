package postgres

import (
	"context"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"16.2", "16.2"},
		{"16", "16"},
		// Packaged builds append vendor detail that core must not try to compare.
		{"16.2 (Debian 16.2-1.pgdg120+2)", "16.2"},
		{"15.6 (Ubuntu 15.6-1.pgdg22.04+1)", "15.6"},
		{"17beta1", "17"},
		{"13.14", "13.14"},
		// Unparseable input is passed through rather than silently becoming an empty string, so a
		// surprising version is visible instead of invisible.
		{"unknown", "unknown"},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			if got := normalizeVersion(tc.raw); got != tc.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCapabilitiesAreCoherent(t *testing.T) {
	caps, err := New().Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if err := sdk.ValidateCapabilities(caps); err != nil {
		t.Fatalf("capabilities are not internally coherent: %v", err)
	}

	if caps.GetEngineType() != EngineType {
		t.Errorf("engine_type = %q, want %q", caps.GetEngineType(), EngineType)
	}
	if caps.GetContractVersion() != version.ContractVersion {
		t.Errorf("contract_version = %q, want %q", caps.GetContractVersion(), version.ContractVersion)
	}
}

// TestCapabilitiesDeclareOnlyWhatIsImplemented guards the rule that a capability is a promise core
// relies on when deciding what to do to a production database. Declaring one before the behaviour
// exists produces its failure during a recovery, which is the worst possible moment.
//
// When a later slice implements one of these, it flips the flag and moves the line here.
func TestCapabilitiesDeclareOnlyWhatIsImplemented(t *testing.T) {
	caps, err := New().Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	implemented := map[string]bool{
		"supports_schema_discovery": caps.GetSupportsSchemaDiscovery(),
		"supports_replication":      caps.GetSupportsReplication(),
		"supports_replication_lag":  caps.GetSupportsReplicationLag(),
	}
	for name, on := range implemented {
		if !on {
			t.Errorf("%s should be declared: Discover and HealthCheck implement it", name)
		}
	}

	notYet := map[string]bool{
		"supports_pitr":                  caps.GetSupportsPitr(),
		"supports_point_in_time_restore": caps.GetSupportsPointInTimeRestore(),
		"supports_sandbox_restore":       caps.GetSupportsSandboxRestore(),
		"supports_online_backup":         caps.GetSupportsOnlineBackup(),
		"supports_config_read":           caps.GetSupportsConfigRead(),
		"supports_storage_metrics":       caps.GetSupportsStorageMetrics(),
	}
	for name, on := range notYet {
		if on {
			t.Errorf("%s is declared but not implemented yet", name)
		}
	}

	if len(caps.GetBackupMethods()) != 0 {
		t.Error("backup methods are declared but Backup is not implemented yet")
	}
	if caps.GetPrincipalModel() != fwv1.PrincipalModel_PRINCIPAL_MODEL_UNSPECIFIED {
		t.Error("a principal model is declared but ListPrincipals is not implemented yet")
	}
}

// TestUnimplementedRPCsRefuseCleanly checks that what the plugin has not built yet is refused with a
// typed error rather than a nil response, a panic, or a hang. Core relies on the distinction
// between "this engine cannot" and "this is broken".
func TestUnimplementedRPCsRefuseCleanly(t *testing.T) {
	p := New()
	ctx := context.Background()

	if _, err := p.VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{}); err == nil {
		t.Error("VerifyRestore should be refused")
	} else if pe := sdk.AsPluginError(err); pe.GetCode() != fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
		t.Errorf("VerifyRestore code = %v, want UNSUPPORTED", pe.GetCode())
	}

	// PITR is the exception: an engine without it answers with an unavailable window and a reason,
	// so the UI can explain the absence rather than showing a failure.
	window, err := p.ListPITRTargets(ctx, &fwv1.ListPITRTargetsRequest{})
	if err != nil {
		t.Fatalf("ListPITRTargets should answer, not fail: %v", err)
	}
	if window.GetAvailable() {
		t.Error("PITR reported available, but it is not implemented")
	}
	if window.GetUnavailableReason() == "" {
		t.Error("unavailable_reason is empty; the UI has nothing to explain")
	}
}
