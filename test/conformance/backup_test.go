//go:build conformance

// The capability-gated backup → restore → verify path, run against every plugin that claims it.
//
// This is the loop the product exists for, driven exactly the way the control plane drives it: a
// real database is dumped through the plugin, the artifact is assembled from presigned part grants
// in a real object store, a throwaway container is provisioned from the plugin's own
// SandboxTemplate, the artifact is restored into it, and the restored copy is compared against the
// manifest captured when the dump was taken.
//
// Nothing here names an engine. What a plugin has to do to be exercised by it is in
// docs/dev/writing-an-engine-plugin.md §10.
package conformance

import (
	"context"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// caseTimeout bounds one end-to-end case. Generous on purpose: the first case on a cold machine
// pays for pulling an engine image and for the engine's own first-boot initialization.
const caseTimeout = 20 * time.Minute

// TestBackupRestoreVerify is the whole loop on a healthy artifact, and the baseline every failure
// case in corruption_test.go is measured against. A suite that only proved the failures would not
// distinguish a working verification from one that reports FAILED unconditionally.
func TestBackupRestoreVerify(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		fixture := requireVerifiablePlugin(t, h, engineType, caps)
		store := objectStore(t)

		ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
		defer cancel()

		source := newInstance(t, engineType, caps, "source")
		if err := fixture.Seed(ctx, source); err != nil {
			t.Fatalf("seed the source instance: %v", err)
		}

		method := sdk.DefaultBackupMethod(caps)
		backup := runBackup(t, ctx, store, client, caps, source, "artifact")

		target := newInstance(t, engineType, caps, "target")
		restoreInto(t, ctx, client, store, backup, sandboxTarget(target), method.GetId())

		verified, err := client.VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
			VerificationId: "conformance-verify",
			Target:         sandboxTarget(target),
			Expected:       backup.result.GetManifest(),
			BackupId:       "conformance-backup",
		})
		if err != nil {
			t.Fatalf("VerifyRestore: %v", err)
		}

		if verified.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
			t.Fatalf("status = %s on a healthy artifact, want VERIFIED: %s\n%s",
				verified.GetStatus(), verified.GetError().GetMessage(), verified.GetReport())
		}

		// Every check the plugin publishes must actually have run. A plugin that declares four and
		// runs one would show the operator a green verification covering less than it claims.
		ran := make(map[fwv1.VerificationCheck]bool, len(verified.GetChecks()))
		for _, check := range verified.GetChecks() {
			ran[check.GetCheck()] = true
			if !check.GetPassed() {
				t.Errorf("%s failed on a healthy restore: %s", check.GetCheck(), check.GetMessage())
			}
			if len(check.GetDiscrepancies()) > 0 {
				t.Errorf("%s reported %d discrepancies on a healthy restore",
					check.GetCheck(), len(check.GetDiscrepancies()))
			}
		}
		for _, declared := range caps.GetSupportedVerificationChecks() {
			if !ran[declared] {
				t.Errorf("the plugin declares the %s check but did not run it", declared)
			}
		}

		if verified.GetReport() == "" {
			t.Error("no report was produced; the UI has nothing to show an operator")
		}
		if verified.GetDuration().AsDuration() <= 0 {
			t.Error("the verification reports no duration")
		}
	})
}

// TestAManifestlessBackupIsInconclusive covers the answer that looks harmless and is not.
//
// Comparing zero objects to zero objects succeeds trivially, so the naive implementation reports
// VERIFIED for a backup that proves nothing at all — a green checkmark on an artifact nobody has
// checked. It runs against the same live, correctly restored instance the happy path produced, so
// the refusal can only be about the manifest.
func TestAManifestlessBackupIsInconclusive(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		fixture := requireVerifiablePlugin(t, h, engineType, caps)
		store := objectStore(t)

		ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
		defer cancel()

		source := newInstance(t, engineType, caps, "source")
		if err := fixture.Seed(ctx, source); err != nil {
			t.Fatalf("seed the source instance: %v", err)
		}

		backup := runBackup(t, ctx, store, client, caps, source, "artifact")
		target := newInstance(t, engineType, caps, "target")
		restoreInto(t, ctx, client, store, backup, sandboxTarget(target), sdk.DefaultBackupMethod(caps).GetId())

		for _, tc := range []struct {
			name     string
			expected *fwv1.SourceManifest
		}{
			{name: "no manifest at all", expected: nil},
			{name: "a manifest with no entries", expected: &fwv1.SourceManifest{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				verified, err := client.VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
					VerificationId: "conformance-verify-no-manifest",
					Target:         sandboxTarget(target),
					Expected:       tc.expected,
				})
				if err != nil {
					// A refusal is acceptable; silently succeeding is not. What must never happen
					// is a VERIFIED status, which this branch cannot produce.
					return
				}
				if verified.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
					t.Fatalf("status = %s without a manifest, want INCONCLUSIVE: %s",
						verified.GetStatus(), verified.GetReport())
				}
			})
		}
	})
}
