//go:build conformance

package conformance

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// TestBackupHistoryIsObservable is the merge gate for ADR-0015's second origin.
//
// It is capability-gated like every other case here: a plugin that does not declare
// backup_history.supported is skipped, and adding a new RPC to the contract is the one thing that
// legitimately adds a case to this suite. What must never happen is an existing assertion moving to
// accommodate an engine.
//
// The shape is deliberately not "call it once and see a row". A poll that runs every half hour for
// a year calls this thousands of times against the same unchanged evidence, so the property that
// actually matters is that the same backup keeps the same identity — otherwise Fleetward's own
// database fills with duplicates of one nightly backup, and the estate looks busier than it is
// (ADR-0027).
func TestBackupHistoryIsObservable(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		history := caps.GetBackupHistory()
		if !history.GetSupported() {
			t.Skipf("%s does not claim it can see backups it did not take", engineType)
		}
		if history.GetSourceDescription() == "" {
			t.Fatal("backup history is claimed but the plugin will not say what it reads, so no " +
				"report could tell an operator where the answer came from")
		}
		backuper := requireExternalBackuper(t, h, engineType)

		ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
		defer cancel()

		source := newInstance(t, engineType, caps, "observed-source")
		if history.GetRequiresSharedDirectory() {
			attachSharedDirectory(t, source)
		}
		if err := backuper.Seed(ctx, source); err != nil {
			t.Fatalf("seed the source instance: %v", err)
		}

		before := listHistory(t, ctx, client, source, time.Time{})

		// A backup taken the way somebody's cron job would take it: by the engine's own means, with
		// Fleetward not involved and nothing to tell it that this happened.
		takenAt := time.Now().UTC()
		if err := backuper.TakeExternalBackup(ctx, source); err != nil {
			t.Fatalf("take a backup outside Fleetward: %v", err)
		}

		after := listHistory(t, ctx, client, source, time.Time{})
		fresh := addedRecords(before, after)
		if len(fresh) == 0 {
			t.Fatalf("a backup was taken on the instance and %s reported none: the evidence this "+
				"plugin reads (%s) did not show it", engineType, history.GetSourceDescription())
		}
		record := fresh[len(fresh)-1]

		// The identity. Everything downstream upserts on it, so an empty one means every poll
		// inserts the same backup again, forever.
		if record.GetExternalId() == "" {
			t.Error("the record carries no external_id, so nothing could tell this backup from the " +
				"next observation of the same backup")
		}
		if record.GetFinishedAt() == nil {
			t.Fatal("the record carries no finish time, so no compliance window could contain it")
		}

		// The timestamp is UTC or it is nothing. An engine that records local time with no offset is
		// the plugin's problem to convert; a plugin that hands the local reading over unchanged
		// produces adherence answers wrong by however far the server is from UTC.
		finished := record.GetFinishedAt().AsTime()
		if finished.Before(takenAt.Add(-2*time.Hour)) || finished.After(time.Now().UTC().Add(2*time.Hour)) {
			t.Errorf("finished_at = %s for a backup taken at %s: the contract's timestamps are UTC, "+
				"and this one is not (approximate = %t)",
				finished.Format(time.RFC3339), takenAt.Format(time.RFC3339),
				record.GetFinishedAtIsApproximate())
		}

		// What the plugin declared about its source has to match what it actually reports. A source
		// that cannot report an outcome must not produce records claiming one, because that claim is
		// what a green tick on somebody's estate would rest on (ADR-0015).
		if !history.GetReportsOutcome() &&
			record.GetOutcome() != fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN {
			t.Errorf("outcome = %s from a source that declares it cannot report one",
				record.GetOutcome())
		}
		if record.GetOutcome() == fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNSPECIFIED {
			t.Error("outcome is unset; UNKNOWN is the value that says the evidence cannot tell")
		}

		// The property this case exists for. Reading the same evidence again must produce the same
		// identity, exactly once.
		repeat := listHistory(t, ctx, client, source, time.Time{})
		seen := 0
		for _, r := range repeat {
			if r.GetExternalId() == record.GetExternalId() {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("re-reading the same evidence found the backup %d times, want exactly 1: a "+
				"poll running every half hour would record this backup %d times a day", seen, seen*48)
		}

		// And `since` is a filter rather than a suggestion. Core polls with a watermark on every
		// run, and a plugin that ignores it scans the engine's whole backup history each time.
		later := listHistory(t, ctx, client, source, finished.Add(time.Second))
		for _, r := range later {
			if r.GetExternalId() == record.GetExternalId() {
				t.Error("a record older than `since` was returned; core's watermark would never advance")
			}
		}
	})
}

// TestBackupHistoryIsRefusedWhenNotDeclared is the other half of the capability, and it runs for
// every plugin that has not turned this on.
//
// "There is no evidence here" and "there were no backups" are different statements. A plugin that
// answered with an empty list instead of refusing would make an engine nobody is watching look like
// an engine with nothing to report, which is the single most dangerous answer in this product.
func TestBackupHistoryIsRefusedWhenNotDeclared(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		if caps.GetBackupHistory().GetSupported() {
			t.Skipf("%s implements backup history; the refusal is not its case", engineType)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.ListBackupHistory(ctx, &fwv1.ListBackupHistoryRequest{
			Credentials: &fwv1.Credentials{Host: "127.0.0.1", Port: 1},
		})
		if err == nil {
			t.Fatalf("ListBackupHistory answered with %d records on a plugin that declares it cannot "+
				"see any", len(resp.GetBackups()))
		}
		if pe, ok := sdk.PluginErrorFrom(err); ok &&
			pe.GetCode() != fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
			t.Errorf("code = %s, want UNSUPPORTED: %s", pe.GetCode(), pe.GetMessage())
		}
	})
}

// ExternalBackuper is the hook a fixture implements when its plugin can observe backup history.
//
// It exists for the same reason Fixture does (ADR-0023): Fleetward never writes to a monitored
// instance, so there is no RPC in the contract that could cause a backup nothing to do with
// Fleetward — and without one, this case could only ever observe backups Fleetward took, which is
// the one thing observation is not for.
type ExternalBackuper interface {
	Fixture

	// TakeExternalBackup causes a backup by the engine's own means, the way a cron job or a script
	// that predates Fleetward would. It must leave evidence where the plugin looks for it, and it
	// must tell Fleetward nothing.
	TakeExternalBackup(ctx context.Context, creds *fwv1.Credentials) error
}

// requireExternalBackuper does not skip a plugin whose declared tools are missing, and that is the
// difference from requireVerifiablePlugin rather than an omission.
//
// required_tools is what a plugin needs in order to take and restore a backup. Observing one is
// reading evidence — a query, or a directory listing — and a plugin that cannot back this instance
// up on this host can still answer perfectly well for the backups somebody else took, which is the
// whole premise of the slice.
func requireExternalBackuper(t *testing.T, _ *harness, engineType string) ExternalBackuper {
	t.Helper()

	fixture, ok := fixtures[engineType]
	if !ok {
		t.Skipf("%s has no conformance fixture; see docs/dev/writing-an-engine-plugin.md §10", engineType)
	}
	backuper, ok := fixture.(ExternalBackuper)
	if !ok {
		t.Skipf("%s declares backup history but its fixture cannot take a backup outside Fleetward, "+
			"so there would be nothing to observe", engineType)
	}
	return backuper
}

// attachSharedDirectory gives the instance a directory the plugin can read, which is what core does
// from the connection's own configuration when this runs for real (ADR-0026).
//
// Both names point at the same path here: the fixture and the plugin are processes on this machine,
// so there is nothing for the two paths to differ about.
func attachSharedDirectory(t *testing.T, creds *fwv1.Credentials) {
	t.Helper()
	if creds.GetSharedDirectory().GetLocalPath() != "" {
		return
	}
	dir := t.TempDir()
	creds.SharedDirectory = &fwv1.SharedDirectory{EnginePath: dir, LocalPath: dir}
}

func listHistory(
	t *testing.T,
	ctx context.Context,
	client fwv1.EnginePluginClient,
	creds *fwv1.Credentials,
	since time.Time,
) []*fwv1.ObservedBackup {
	t.Helper()

	req := &fwv1.ListBackupHistoryRequest{Credentials: creds, Limit: 200}
	if !since.IsZero() {
		req.Since = timestamppb.New(since)
	}
	resp, err := client.ListBackupHistory(ctx, req)
	if err != nil {
		t.Fatalf("ListBackupHistory: %v", err)
	}
	return resp.GetBackups()
}

// addedRecords returns the records present in `after` and not in `before`, in the order the plugin
// reported them.
func addedRecords(before, after []*fwv1.ObservedBackup) []*fwv1.ObservedBackup {
	known := make(map[string]bool, len(before))
	for _, r := range before {
		known[r.GetExternalId()] = true
	}
	var out []*fwv1.ObservedBackup
	for _, r := range after {
		if !known[r.GetExternalId()] {
			out = append(out, r)
		}
	}
	return out
}
