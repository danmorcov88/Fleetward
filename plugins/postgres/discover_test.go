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
		// Slice A4: pg_dump runs against a live server and produces an artifact with a manifest.
		"supports_online_backup": caps.GetSupportsOnlineBackup(),
		// Slice A5: the artifact is restored into a sandbox and compared against that manifest.
		"supports_sandbox_restore": caps.GetSupportsSandboxRestore(),
	}
	for name, on := range implemented {
		if !on {
			t.Errorf("%s should be declared: the RPC behind it is implemented", name)
		}
	}

	notYet := map[string]bool{
		"supports_pitr":                  caps.GetSupportsPitr(),
		"supports_point_in_time_restore": caps.GetSupportsPointInTimeRestore(),
		"supports_config_read":           caps.GetSupportsConfigRead(),
		"supports_storage_metrics":       caps.GetSupportsStorageMetrics(),
	}
	for name, on := range notYet {
		if on {
			t.Errorf("%s is declared but not implemented yet", name)
		}
	}

	if len(caps.GetBackupMethods()) != 1 {
		t.Errorf("declared %d backup methods, want exactly the pg_dump method from slice A4",
			len(caps.GetBackupMethods()))
	}
	// The integrity and queryability checks stay undeclared: amcheck is slice A6's territory and
	// nothing represents a "representative read" yet. Declaring a check the plugin cannot run would
	// have core report verification as failing rather than as narrower than it hoped.
	for _, check := range []fwv1.VerificationCheck{
		fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
		fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
	} {
		if !sdk.SupportsCheck(caps, check) {
			t.Errorf("%s should be declared: VerifyRestore implements it", check)
		}
	}
	for _, check := range []fwv1.VerificationCheck{
		fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY,
		fwv1.VerificationCheck_VERIFICATION_CHECK_QUERYABILITY,
	} {
		if sdk.SupportsCheck(caps, check) {
			t.Errorf("%s is declared but not implemented yet", check)
		}
	}
	if caps.GetSandboxTemplate().GetImageRepository() == "" {
		t.Error("sandbox restore is declared without an image repository to stand one up from")
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

	if _, err := p.GetConfig(ctx, &fwv1.GetConfigRequest{}); err == nil {
		t.Error("GetConfig should be refused")
	} else if pe := sdk.AsPluginError(err); pe.GetCode() != fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
		t.Errorf("GetConfig code = %v, want UNSUPPORTED", pe.GetCode())
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
