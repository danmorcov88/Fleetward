//go:build conformance

// The failure cases. This file is why slice A6 exists.
//
// Every check written before it was tested on the happy path, and a verification system that has
// only ever been shown to pass is indistinguishable from one that always passes — which is far
// worse than no verification at all, because it manufactures confidence.
//
// Two rules shape what is written here.
//
// Corruption happens in the bucket, never through the plugin. A truncated object and a flipped
// byte are what bit rot and a half-finished upload actually look like; corruption injected through
// the code path under test can only ever exercise the branch someone remembered to write.
//
// And the answer has to be the right kind of wrong. FAILED is reserved for evidence about the
// artifact; everything else — a sandbox that never answered, a manifest that is not there — is
// INCONCLUSIVE (ADR-0022). A system that reports infrastructure trouble as data loss gets muted,
// and a muted alert is the same as no alert.
package conformance

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/backup"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// TestACorruptedArtifactFails alters the stored object and requires the plugin to refuse it before
// a single statement is applied.
//
// Both cases share one restore target because neither is ever expected to reach it: an artifact
// rejected on its checksum has been rejected before the sandbox is touched. The target is real
// rather than absent precisely so that "it never got there" is a finding rather than an assumption.
func TestACorruptedArtifactFails(t *testing.T) {
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

		method := sdk.DefaultBackupMethod(caps).GetId()
		healthy := runBackup(t, ctx, store, client, caps, source, "artifact")
		target := sandboxTarget(newInstance(t, engineType, caps, "target"))

		tests := []struct {
			name   string
			mutate func([]byte) []byte
		}{
			{
				// A failed upload, or a lifecycle rule that caught an object mid-write. The size
				// changes as well as the hash, so two independent checks could catch it.
				name: "truncated",
				mutate: func(b []byte) []byte {
					return b[:len(b)*3/4]
				},
			},
			{
				// Bit rot: same length, one byte different, deep enough inside that no header
				// check would notice. Nothing but the checksum can catch this one, which is why it
				// is the case that proves the checksum is doing work.
				name: "bytes flipped mid-stream",
				mutate: func(b []byte) []byte {
					b[len(b)/2] ^= 0xFF
					return b
				},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				corrupted := copyArtifact(t, ctx, store, healthy, tc.mutate)

				terminal, err := restore(ctx, client, corrupted,
					artifactSource(t, ctx, store, corrupted), target, method)
				if err != nil {
					t.Fatalf("Restore: %v", err)
				}

				if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_FAILED {
					t.Fatalf("phase = %s on a corrupted artifact, want FAILED", terminal.GetPhase())
				}

				pe := terminal.GetError()
				if !sdk.IsArtifactCorrupt(pe) {
					t.Errorf("the failure does not blame the artifact, so core reports it as an "+
						"infrastructure problem rather than a bad backup: %s / %s",
						pe.GetCode(), pe.GetMessage())
				}
				if got := backup.RestoreFailureStatus(pe); got != fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
					t.Errorf("core would record %s for this failure, want FAILED", got)
				}
			})
		}
	})
}

// TestVerificationFailsWhenTheSourceNoLongerMatchesItsManifest is the case a checksum cannot catch.
//
// Both artifacts here are perfectly intact and both restore cleanly; what is wrong is that the
// manifest describes a database that has since lost rows. It is the same comparison a subtly
// damaged artifact would fail, produced without tampering with either the artifact or the manifest
// — every number in it came from the plugin.
//
// The report must name the object and both counts. An operator woken at 3am needs to know which
// table is short by how many rows, not that "verification failed".
func TestVerificationFailsWhenTheSourceNoLongerMatchesItsManifest(t *testing.T) {
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

		method := sdk.DefaultBackupMethod(caps).GetId()
		first := runBackup(t, ctx, store, client, caps, source, "artifact")

		object, removed, err := fixture.RemoveRows(ctx, source)
		if err != nil {
			t.Fatalf("remove rows from the source: %v", err)
		}
		second := runBackup(t, ctx, store, client, caps, source, "artifact")

		target := newInstance(t, engineType, caps, "target")
		restoreInto(t, ctx, client, store, second, sandboxTarget(target), method)

		verified, err := client.VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
			VerificationId: "conformance-verify-stale-manifest",
			Target:         sandboxTarget(target),
			// The manifest from before the rows went, against the artifact from after.
			Expected: first.result.GetManifest(),
		})
		if err != nil {
			t.Fatalf("VerifyRestore: %v", err)
		}

		if verified.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
			t.Fatalf("status = %s for a restore missing %d rows, want FAILED\n%s",
				verified.GetStatus(), removed, verified.GetReport())
		}

		var found *fwv1.Discrepancy
		for _, check := range verified.GetChecks() {
			for _, d := range check.GetDiscrepancies() {
				if d.GetObjectName() == object {
					found = d
				}
			}
		}
		if found == nil {
			t.Fatalf("no discrepancy names %s; the report says something is wrong without saying "+
				"what\n%s", object, verified.GetReport())
		}
		if found.GetExpected()-found.GetActual() != removed {
			t.Errorf("%s discrepancy = %d expected, %d actual: a difference of %d, want %d",
				object, found.GetExpected(), found.GetActual(),
				found.GetExpected()-found.GetActual(), removed)
		}
	})
}

// TestAnUnreachableTargetIsNeverReportedAsDataLoss is the case that matters as much as the four
// above it, and the one this slice found broken.
//
// A verification pulls an image, starts a database, downloads over a network the plugin does not
// control, shells out to a native tool, and only then compares counts. Exactly one of those steps
// says anything about the backup. A plugin that reports a target which never answered as a tool
// failure fires this product's one differentiating alert on a container that lost a race — and an
// alert that fires routinely is an alert nobody reads.
//
// Port 1 stands in for the sandbox that never became ready: a real address, nothing listening.
func TestAnUnreachableTargetIsNeverReportedAsDataLoss(t *testing.T) {
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

		healthy := runBackup(t, ctx, store, client, caps, source, "artifact")

		// A well-formed target in every respect except that nothing is listening behind it. The
		// credentials are cloned rather than edited in place: they are the source instance's, and
		// the source is still the thing the backup was taken from.
		unreachable := proto.CloneOf(source)
		unreachable.Port = 1
		dead := sandboxTarget(unreachable)

		terminal, err := restore(ctx, client, healthy,
			artifactSource(t, ctx, store, healthy), dead, sdk.DefaultBackupMethod(caps).GetId())
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_FAILED {
			t.Fatalf("phase = %s against a target that does not exist, want FAILED", terminal.GetPhase())
		}

		pe := terminal.GetError()
		if sdk.IsArtifactCorrupt(pe) {
			t.Errorf("the plugin blamed the artifact for an unreachable target: %s", pe.GetMessage())
		}
		if got := backup.RestoreFailureStatus(pe); got != fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
			t.Errorf("core would record %s for an unreachable target, want INCONCLUSIVE. The error "+
				"was %s: %s", got, pe.GetCode(), pe.GetMessage())
		}
	})
}
