//go:build integration

package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
)

// Retention is the first thing Fleetward does that destroys something, so these tests are written
// the way the feature was: every one of them is an attempt to make it delete something it must not.
//
// The object store is real here, not stubbed. "The row says the artifact is gone" is not the claim
// worth testing — "the bytes are gone, and these other bytes are still there" is.

// -------------------------------------------------------------------------------------------------
// The rule that has no exceptions
// -------------------------------------------------------------------------------------------------

// TestRetentionNeverTouchesAnObservedBackup is the most important test in this file.
//
// An observed backup is somebody else's file, and the promise that Fleetward changes nothing on an
// estate it is pointed at is the whole reason anyone installs it (ADR-0015). Both halves are
// asserted, because one of them is a query and the other is the thing that saves us when a future
// query is written by somebody who has not read that record:
//
//	the sweep declines it — even offered an expiry that ran out a year ago;
//	the database refuses the transition outright — even when the UPDATE is written by hand.
func TestRetentionNeverTouchesAnObservedBackup(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	longExpired := time.Now().Add(-365 * 24 * time.Hour)
	observed := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin:      originObserved,
		state:       "succeeded",
		completedAt: longExpired,
		expiresAt:   &longExpired,
		// An observed row carries no object of Fleetward's; external_location is where somebody
		// else's file is. Seeded with an expiry it could never have acquired, precisely so the test
		// cannot pass by accident because the row was ineligible for an unrelated reason.
	})

	// A managed backup beside it, so the sweep has real work to do and the observed row is being
	// declined rather than merely missed by a query that found nothing at all.
	managed := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin:      originManaged,
		state:       "succeeded",
		completedAt: longExpired,
		expiresAt:   &longExpired,
		objectKey:   "tenants/t/instances/i/backups/managed-1/artifact",
	})
	// The floor keeps the most recent successful backup of an instance, so a second, newer one is
	// needed for the old one to be eligible at all.
	newer := time.Now().Add(-time.Hour)
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: newer,
		objectKey: "tenants/t/instances/i/backups/managed-2/artifact",
	})

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.Expired != 1 || result.ArtifactsDeleted != 1 {
		t.Fatalf("the sweep expired %d and deleted %d; exactly the one managed backup should have gone",
			result.Expired, result.ArtifactsDeleted)
	}

	if state := h.backupState(t, ctx, observed); state != "succeeded" {
		t.Fatalf("the observed backup became %q; Fleetward must never delete somebody else's backup", state)
	}
	if state := h.backupState(t, ctx, managed); state != "expired" {
		t.Fatalf("the managed backup is %q, so the sweep did nothing and the test above proves nothing", state)
	}

	// And the barrier that survives a query written six months from now by somebody who has not
	// read ADR-0015. This is a hand-written UPDATE with no filter at all — exactly the mistake the
	// constraint exists for.
	_, err = h.pool.Exec(ctx, `UPDATE backups SET state = 'expired' WHERE id = $1`, observed)
	if err == nil {
		t.Fatal("the database allowed an observed backup to be expired; the CHECK constraint is missing " +
			"and the only thing protecting a customer's own backups is a WHERE clause somebody can forget")
	}
	if !strings.Contains(err.Error(), "backups_observed_never_expires") {
		t.Fatalf("the refusal should name the constraint so the reason is findable; got %v", err)
	}
}

// -------------------------------------------------------------------------------------------------
// What makes an upgrade safe
// -------------------------------------------------------------------------------------------------

// TestABackupWithNoExpiryIsNeverDeleted.
//
// NULL is not "expire by default"; it is never. This is what every backup taken before this slice
// carries, and every manual one, so it is the single property that makes upgrading to this version
// delete nothing at all (ADR-0031).
func TestABackupWithNoExpiryIsNeverDeleted(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	ancient := time.Now().Add(-5 * 365 * 24 * time.Hour)
	noExpiry := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: ancient,
		objectKey: "tenants/t/instances/i/backups/ancient/artifact",
	})
	// Plenty of newer backups, so the floor is not what is keeping it.
	for i := range 3 {
		when := time.Now().Add(-time.Duration(i+1) * time.Hour)
		h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded", completedAt: when,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/recent-%d/artifact", i),
		})
	}

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if !result.Empty() {
		t.Fatalf("the sweep acted on backups that carry no expiry: %+v", result)
	}
	if state := h.backupState(t, ctx, noExpiry); state != "succeeded" {
		t.Fatalf("a five-year-old backup with no declared retention became %q", state)
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/ancient/artifact")
}

// -------------------------------------------------------------------------------------------------
// The artifact actually goes, and the record of it does not
// -------------------------------------------------------------------------------------------------

// TestAnOutlivedBackupLosesItsBytesAndKeepsItsRow.
//
// Three claims in one: the object is really gone from the store, the row is still readable, and the
// row still names where the artifact was. The last one is the audit record — "what once existed" —
// and is why retention never issues a DELETE.
func TestAnOutlivedBackupLosesItsBytesAndKeepsItsRow(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	key := "tenants/t/instances/i/backups/outlived/artifact"
	past := time.Now().Add(-40 * 24 * time.Hour)
	outlived := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past,
		objectKey: key, sizeBytes: 4096,
	})
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: time.Now().Add(-time.Hour),
		objectKey: "tenants/t/instances/i/backups/keeper/artifact",
	})

	h.requireObjectPresent(t, ctx, key)

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.ArtifactsDeleted != 1 || result.BytesReclaimed != 4096 {
		t.Fatalf("sweep reported %d artifacts and %d bytes, want 1 and 4096",
			result.ArtifactsDeleted, result.BytesReclaimed)
	}

	h.requireObjectAbsent(t, ctx, key)

	var (
		state, bucket, objectKey string
		deletedAt                *time.Time
	)
	if err := h.pool.QueryRow(ctx, `
		SELECT state, bucket, object_key, artifact_deleted_at FROM backups WHERE id = $1`, outlived).
		Scan(&state, &bucket, &objectKey, &deletedAt); err != nil {
		t.Fatalf("the row is unreadable after retention; it must survive as the record of what existed: %v", err)
	}
	if state != "expired" {
		t.Fatalf("state = %q, want expired", state)
	}
	if objectKey != key || bucket != testBucket {
		t.Fatalf("the row forgot where the artifact was (%s/%s); an audit cannot show what once existed",
			bucket, objectKey)
	}
	if deletedAt == nil {
		t.Fatal("artifact_deleted_at was not written, so the next sweep will queue this row forever")
	}

	// And a second sweep must be a no-op rather than re-queueing the same row.
	again, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("second SweepRetention: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("a second sweep found work to do on a finished row: %+v", again)
	}
}

// -------------------------------------------------------------------------------------------------
// The floor
// -------------------------------------------------------------------------------------------------

// TestTheFloorKeepsTheLastSuccessfulBackupOfAnInstance.
//
// Every backup of this instance is long past its expiry. A purely time-based implementation deletes
// all of them and leaves a production server with nothing — which is what a correct reading of
// "delete anything older than 30 days" actually does (ADR-0032).
func TestTheFloorKeepsTheLastSuccessfulBackupOfAnInstance(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	var newest string
	for i := range 4 {
		when := time.Now().Add(-time.Duration(40-i) * 24 * time.Hour)
		id := h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded", completedAt: when, expiresAt: &when,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/old-%d/artifact", i),
		})
		newest = id // the loop walks forward in time, so the last one is the most recent
	}

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.ArtifactsDeleted != 3 {
		t.Fatalf("deleted %d artifacts; three of the four should go and the newest must stay",
			result.ArtifactsDeleted)
	}
	if state := h.backupState(t, ctx, newest); state != "succeeded" {
		t.Fatalf("the instance's most recent successful backup became %q; the estate now has nothing "+
			"to restore from", state)
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/old-3/artifact")
}

// TestTheFloorKeepsTheLastBackupProvenRestorable is the case the floor exists for, and the one a
// single-rule floor gets wrong.
//
// The instance has been backing up successfully for weeks and failing verification for weeks. Rule
// one keeps the newest backup — which is known to be unrestorable. Rule two keeps the last one that
// was actually proven good, which is the only artifact on this server worth anything.
func TestTheFloorKeepsTheLastBackupProvenRestorable(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := func(days int) time.Time { return time.Now().Add(-time.Duration(days) * 24 * time.Hour) }

	verifiedWhen := past(40)
	lastGood := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: verifiedWhen, expiresAt: &verifiedWhen,
		objectKey:    "tenants/t/instances/i/backups/last-good/artifact",
		verification: "verified",
	})

	// Three newer backups, every one of them proven bad.
	var newest string
	for i := range 3 {
		when := past(10 - i)
		newest = h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded", completedAt: when, expiresAt: &when,
			objectKey:    fmt.Sprintf("tenants/t/instances/i/backups/bad-%d/artifact", i),
			verification: "failed",
		})
	}

	if _, err := h.svc.SweepRetention(ctx); err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}

	if state := h.backupState(t, ctx, lastGood); state != "succeeded" {
		t.Fatalf("the last backup proven restorable became %q. Every other artifact on this instance "+
			"is known to be unrestorable, so this deleted the only thing that could have saved it", state)
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/last-good/artifact")

	if state := h.backupState(t, ctx, newest); state != "succeeded" {
		t.Fatalf("the newest backup became %q; rule one should have kept it too", state)
	}
	// The two in between are unprotected and go, which is what makes this a floor rather than a
	// refusal to delete anything on a sick instance.
	h.requireObjectAbsent(t, ctx, "tenants/t/instances/i/backups/bad-0/artifact")
}

// TestTheFloorWidensWithMinKeep. One knob, and the sweep honours it.
func TestTheFloorWidensWithMinKeep(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	for i := range 5 {
		when := time.Now().Add(-time.Duration(40-i) * 24 * time.Hour)
		h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded", completedAt: when, expiresAt: &when,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/n-%d/artifact", i),
		})
	}

	svc := h.withRetention(t, RetentionPolicy{Enabled: true, Interval: time.Hour, MinKeep: 3, MaxPerSweep: 500})
	result, err := svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.ArtifactsDeleted != 2 {
		t.Fatalf("deleted %d of five with a floor of three; two should go", result.ArtifactsDeleted)
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/n-2/artifact")
	h.requireObjectAbsent(t, ctx, "tenants/t/instances/i/backups/n-0/artifact")
}

// TestTheFloorIsPerInstance. One instance's healthy backup history must not make another instance's
// last backup deletable.
func TestTheFloorIsPerInstance(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	second := h.seedSecondInstance(t, ctx, "prod-2")
	past := time.Now().Add(-40 * 24 * time.Hour)

	only := h.seedRetentionBackup(t, ctx, retentionSeed{
		instanceID: second, origin: originManaged, state: "succeeded",
		completedAt: past, expiresAt: &past,
		objectKey: "tenants/t/instances/second/backups/only/artifact",
	})
	for i := range 3 {
		when := time.Now().Add(-time.Duration(40-i) * 24 * time.Hour)
		h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded", completedAt: when, expiresAt: &when,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/first-%d/artifact", i),
		})
	}

	if _, err := h.svc.SweepRetention(ctx); err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if state := h.backupState(t, ctx, only); state != "succeeded" {
		t.Fatalf("the second instance's only backup became %q because the first instance had plenty", state)
	}
}

// -------------------------------------------------------------------------------------------------
// The race the lease does not cover
// -------------------------------------------------------------------------------------------------

// TestABackupBeingVerifiedIsNotDeletedUnderneathIt.
//
// A verification downloads the artifact when it runs. Retention deleting it in between is a real
// race, and the lease machinery does not cover it: the lease is on a job row and this is a
// different row. Both halves of the guard are exercised, because a queued verification has a job
// before it has a verifications row — so checking only the latter leaves the window open for
// exactly as long as the job sits pending.
func TestABackupBeingVerifiedIsNotDeletedUnderneathIt(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := time.Now().Add(-40 * 24 * time.Hour)

	beingVerified := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past,
		objectKey: "tenants/t/instances/i/backups/verifying/artifact", verification: "running",
	})
	queuedForVerification := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past,
		objectKey: "tenants/t/instances/i/backups/queued/artifact",
	})
	h.seedPendingVerifyJob(t, ctx, queuedForVerification)

	// A newer one so the floor is not what is doing the work here.
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: time.Now().Add(-time.Hour),
		objectKey: "tenants/t/instances/i/backups/newest/artifact",
	})

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if !result.Empty() {
		t.Fatalf("the sweep acted on a backup something was reading: %+v", result)
	}

	for _, id := range []string{beingVerified, queuedForVerification} {
		if state := h.backupState(t, ctx, id); state != "succeeded" {
			t.Fatalf("backup %s became %q while it was being verified", id, state)
		}
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/verifying/artifact")
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/queued/artifact")

	// And once the verification concludes, the same backup becomes eligible. The guard is a delay,
	// not a reprieve.
	if _, err := h.pool.Exec(ctx,
		`UPDATE verifications SET status = 'verified', completed_at = now() WHERE backup_id = $1`,
		beingVerified); err != nil {
		t.Fatalf("conclude the verification: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE jobs SET state = 'succeeded', finished_at = now() WHERE payload->>'backup_id' = $1`,
		queuedForVerification); err != nil {
		t.Fatalf("conclude the verify job: %v", err)
	}

	after, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention after the verification finished: %v", err)
	}
	// The one that came back VERIFIED is now held by the floor's second rule instead, so only the
	// other one goes. That is the design, and asserting the exact number is what would catch the
	// two rules being collapsed into one.
	if after.ArtifactsDeleted != 1 {
		t.Fatalf("deleted %d after the verifications concluded; the queued one should go and the "+
			"one proven restorable should be held by the floor", after.ArtifactsDeleted)
	}
	h.requireObjectAbsent(t, ctx, "tenants/t/instances/i/backups/queued/artifact")
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/verifying/artifact")
}

// -------------------------------------------------------------------------------------------------
// Interruption and concurrency
// -------------------------------------------------------------------------------------------------

// TestAnInterruptedSweepIsFinishedByTheNextOne.
//
// A control plane killed between marking a backup expired and deleting its object leaves a row that
// says expired with its bytes still there. The design's answer is that the leftover is
// self-reconciling rather than reported: the next sweep — on any control plane, not necessarily the
// one that died — selects on exactly that state and finishes.
//
// The interruption is simulated by writing the state the crash would have left, which is the same
// row a killed process leaves and is deterministic in a way killing a goroutine is not.
func TestAnInterruptedSweepIsFinishedByTheNextOne(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	key := "tenants/t/instances/i/backups/half-done/artifact"
	past := time.Now().Add(-40 * 24 * time.Hour)
	id := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past,
		objectKey: key, sizeBytes: 2048,
	})

	// Step one happened; step two did not.
	if _, err := h.pool.Exec(ctx,
		`UPDATE backups SET state = 'expired', updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("simulate the interruption: %v", err)
	}
	h.requireObjectPresent(t, ctx, key)

	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.Expired != 0 {
		t.Fatalf("the sweep expired %d rows; the row was already expired and must not be counted twice",
			result.Expired)
	}
	if result.ArtifactsDeleted != 1 || result.BytesReclaimed != 2048 {
		t.Fatalf("the leftover was not finished: %+v", result)
	}
	h.requireObjectAbsent(t, ctx, key)

	third, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("third SweepRetention: %v", err)
	}
	if !third.Empty() {
		t.Fatalf("a third sweep still found work: %+v", third)
	}
}

// TestTwoConcurrentSweepsDeleteEachArtifactOnce.
//
// This is the property that lets retention run without a lease. The state transition is its own
// guard: a row is expired by exactly one of two concurrent sweeps, and the other matches nothing.
// Deleting an object that is already gone is not an error, so the overlap costs a wasted call and
// nothing else.
func TestTwoConcurrentSweepsDeleteEachArtifactOnce(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := time.Now().Add(-40 * 24 * time.Hour)
	const count = 6
	for i := range count {
		h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded",
			completedAt: past.Add(time.Duration(i) * time.Minute), expiresAt: &past,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/race-%d/artifact", i),
		})
	}
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: time.Now().Add(-time.Hour),
		objectKey: "tenants/t/instances/i/backups/race-keeper/artifact",
	})

	second := h.withRetention(t, RetentionPolicy{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		expired int
		errs    []error
	)
	for _, svc := range []*Service{h.svc, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.SweepRetention(ctx)
			mu.Lock()
			defer mu.Unlock()
			expired += result.Expired
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("a concurrent sweep failed: %v", errs)
	}
	// The sum across both sweeps is the assertion that matters: a row counted twice would mean two
	// processes both believed they had expired it.
	if expired != count {
		t.Fatalf("two sweeps expired %d rows between them, want exactly %d", expired, count)
	}
	for i := range count {
		h.requireObjectAbsent(t, ctx, fmt.Sprintf("tenants/t/instances/i/backups/race-%d/artifact", i))
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/race-keeper/artifact")
}

// TestASweepDeletesNoMoreThanItsCeiling. The bound exists so that a bug is bounded, and a bound
// nobody tests is a bound somebody removes.
func TestASweepDeletesNoMoreThanItsCeiling(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := time.Now().Add(-40 * 24 * time.Hour)
	for i := range 5 {
		h.seedRetentionBackup(t, ctx, retentionSeed{
			origin: originManaged, state: "succeeded",
			completedAt: past.Add(time.Duration(i) * time.Minute), expiresAt: &past,
			objectKey: fmt.Sprintf("tenants/t/instances/i/backups/cap-%d/artifact", i),
		})
	}
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: time.Now().Add(-time.Hour),
		objectKey: "tenants/t/instances/i/backups/cap-keeper/artifact",
	})

	svc := h.withRetention(t, RetentionPolicy{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 2})
	result, err := svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.Expired != 2 || result.ArtifactsDeleted != 2 {
		t.Fatalf("a sweep bounded at two expired %d and deleted %d", result.Expired, result.ArtifactsDeleted)
	}

	// And the rest is not lost — it goes on the following sweeps.
	total := result.ArtifactsDeleted
	for range 3 {
		next, err := svc.SweepRetention(ctx)
		if err != nil {
			t.Fatalf("SweepRetention: %v", err)
		}
		total += next.ArtifactsDeleted
	}
	if total != 5 {
		t.Fatalf("successive bounded sweeps removed %d of five; the backlog must drain", total)
	}
}

// -------------------------------------------------------------------------------------------------
// The preview, which is the only surface an operator gets
// -------------------------------------------------------------------------------------------------

// TestThePreviewSaysExactlyWhatTheSweepThenDoes.
//
// There is no job row behind a sweep, so the preview is what an operator reads before enabling
// retention. A preview that answered a slightly different question would be worse than none,
// because it would be believed — so the assertion is not that the preview looks sensible, it is
// that the sweep does precisely what the preview said.
func TestThePreviewSaysExactlyWhatTheSweepThenDoes(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := func(days int) time.Time { return time.Now().Add(-time.Duration(days) * 24 * time.Hour) }
	old40, old30, old20 := past(40), past(30), past(20)

	goes := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: old40, expiresAt: &old40,
		objectKey: "tenants/t/instances/i/backups/goes/artifact", sizeBytes: 1024,
	})
	verified := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: old30, expiresAt: &old30,
		objectKey: "tenants/t/instances/i/backups/verified/artifact", verification: "verified",
	})
	newest := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: old20, expiresAt: &old20,
		objectKey: "tenants/t/instances/i/backups/newest/artifact",
	})
	// An observed backup must not appear anywhere in the preview, in either list. A DBA reading
	// "kept because…" about their own file would rightly ask why Fleetward had an opinion at all.
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originObserved, state: "succeeded", completedAt: old40, expiresAt: &old40,
	})

	preview, err := h.svc.PreviewRetention(ctx, PreviewRetentionInput{})
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	if got := idsOf(preview.GetExpiring()); len(got) != 1 || got[0] != goes {
		t.Fatalf("expiring = %v, want exactly [%s]", got, goes)
	}
	if preview.GetReclaimableBytes() != 1024 {
		t.Fatalf("reclaimable = %d, want 1024", preview.GetReclaimableBytes())
	}

	protected := map[string]string{}
	for _, c := range preview.GetProtected() {
		protected[c.GetBackupId()] = c.GetProtectedReason()
	}
	if len(protected) != 2 {
		t.Fatalf("protected = %v, want the newest and the last verified one", protected)
	}
	if !strings.Contains(protected[newest], "most recent successful backup") {
		t.Errorf("the newest backup's reason does not explain the floor: %q", protected[newest])
	}
	if !strings.Contains(protected[verified], "proven restorable") {
		t.Errorf("the verified backup's reason does not explain why it is kept: %q", protected[verified])
	}
	for _, c := range append(preview.GetExpiring(), preview.GetProtected()...) {
		if c.GetInstanceName() == "" {
			t.Errorf("candidate %s has no instance name, so the operator cannot tell which server it is",
				c.GetBackupId())
		}
	}

	// Now run the sweep and hold it to what the preview promised.
	result, err := h.svc.SweepRetention(ctx)
	if err != nil {
		t.Fatalf("SweepRetention: %v", err)
	}
	if result.ArtifactsDeleted != 1 {
		t.Fatalf("the preview named 1 artifact and the sweep deleted %d", result.ArtifactsDeleted)
	}
	if state := h.backupState(t, ctx, goes); state != "expired" {
		t.Fatalf("the backup the preview named is %q", state)
	}
	for _, id := range []string{verified, newest} {
		if state := h.backupState(t, ctx, id); state != "succeeded" {
			t.Fatalf("the preview said %s would stay and it is now %q", id, state)
		}
	}
}

// TestThePreviewShowsWhatAnInterruptedSweepLeftBehind. The third list is normally empty, and when it
// is not it is the only place that state is visible.
func TestThePreviewShowsWhatAnInterruptedSweepLeftBehind(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	key := "tenants/t/instances/i/backups/left-behind/artifact"
	past := time.Now().Add(-40 * 24 * time.Hour)
	id := h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past, objectKey: key,
	})
	if _, err := h.pool.Exec(ctx,
		`UPDATE backups SET state = 'expired', updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("simulate the interruption: %v", err)
	}

	preview, err := h.svc.PreviewRetention(ctx, PreviewRetentionInput{})
	if err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}
	if got := idsOf(preview.GetPendingDeletion()); len(got) != 1 || got[0] != id {
		t.Fatalf("pending deletion = %v, want [%s]", got, id)
	}
}

// TestThePreviewIsAvailableWhileRetentionIsDisabled, and says so.
//
// The person most likely to run this command is the one deciding whether to enable retention at
// all, so refusing to answer until it is enabled would withhold the answer from exactly the reader
// it is for.
func TestThePreviewIsAvailableWhileRetentionIsDisabled(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	past := time.Now().Add(-40 * 24 * time.Hour)
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: past, expiresAt: &past,
		objectKey: "tenants/t/instances/i/backups/disabled/artifact",
	})
	h.seedRetentionBackup(t, ctx, retentionSeed{
		origin: originManaged, state: "succeeded", completedAt: time.Now().Add(-time.Hour),
		objectKey: "tenants/t/instances/i/backups/disabled-keeper/artifact",
	})

	svc := h.withRetention(t, RetentionPolicy{Enabled: false, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500})

	preview, err := svc.PreviewRetention(ctx, PreviewRetentionInput{})
	if err != nil {
		t.Fatalf("PreviewRetention with retention disabled: %v", err)
	}
	if preview.GetPolicy().GetEnabled() {
		t.Fatal("the preview claims retention is enabled when it is not; the reader would expect the " +
			"listed artifacts to disappear")
	}
	if len(preview.GetExpiring()) != 1 {
		t.Fatalf("expiring = %d, want 1: the preview must answer even while the sweep is off",
			len(preview.GetExpiring()))
	}

	if result, err := svc.SweepRetention(ctx); err != nil || !result.Empty() {
		t.Fatalf("a disabled sweep did something: %+v, %v", result, err)
	}
	h.requireObjectPresent(t, ctx, "tenants/t/instances/i/backups/disabled/artifact")
}

// -------------------------------------------------------------------------------------------------
// The other end: where the expiry comes from
// -------------------------------------------------------------------------------------------------

// TestAScheduledBackupIsStampedWithItsSchedulesRetention closes the loop.
//
// The sweep is only correct if something writes expires_at, and a manual run must not get one. Both
// halves run a real backup through the real service.
func TestAScheduledBackupIsStampedWithItsSchedulesRetention(t *testing.T) {
	ctx := testTenantCtx()
	h := newHarness(t)

	// A real artifact, so this exercises the same recordSuccess path a production backup does.
	h.plugin.payload = []byte(strings.Repeat("fleetward-artifact-bytes ", 1<<10))

	scheduled, _, err := h.svc.RunBackup(ctx, RunBackupInput{
		InstanceID:    h.instance,
		RetentionDays: 7,
	})
	if err != nil {
		t.Fatalf("scheduled backup: %v", err)
	}
	final := h.waitForState(t, scheduled)
	expires := h.expiresAt(t, ctx, scheduled)
	if expires == nil {
		t.Fatalf("a backup taken under a schedule with seven days' retention carries no expiry, so "+
			"retention will never remove it (backup state: %s, error: %q)",
			final.GetState(), final.GetErrorMessage())
	}
	if want := time.Now().AddDate(0, 0, 7); expires.Sub(want) > time.Minute || want.Sub(*expires) > time.Minute {
		t.Fatalf("expiry %v is not seven days out (%v)", expires, want)
	}

	manual, _, err := h.svc.RunBackup(ctx, RunBackupInput{
		InstanceID:        h.instance,
		TriggeredManually: true,
	})
	if err != nil {
		t.Fatalf("manual backup: %v", err)
	}
	h.waitForState(t, manual)
	if got := h.expiresAt(t, ctx, manual); got != nil {
		t.Fatalf("a manually triggered backup was stamped to expire at %v. Nothing declared a "+
			"retention for it, and Fleetward must not invent one in order to delete something", got)
	}
}

// -------------------------------------------------------------------------------------------------
// Harness
// -------------------------------------------------------------------------------------------------

type retentionSeed struct {
	instanceID  string
	origin      string
	state       string
	completedAt time.Time
	expiresAt   *time.Time
	// objectKey, when set, is written to the object store as well as the row, so that a test can
	// assert on bytes rather than on bookkeeping. Left empty for an observed backup, which owns no
	// object of Fleetward's.
	objectKey string
	sizeBytes int64
	// verification, when set, inserts a verifications row with that status.
	verification string
}

// seedRetentionBackup writes one backup row and, when it names an object, the object behind it.
func (h *harness) seedRetentionBackup(t *testing.T, ctx context.Context, seed retentionSeed) string {
	t.Helper()

	if seed.instanceID == "" {
		seed.instanceID = h.instance
	}
	if seed.sizeBytes == 0 {
		seed.sizeBytes = 1024
	}

	bucket := ""
	if seed.objectKey != "" {
		bucket = testBucket
		body := strings.NewReader(strings.Repeat("x", int(seed.sizeBytes)))
		if _, err := h.store.Put(ctx, seed.objectKey, body, seed.sizeBytes, objstore.PutOptions{}); err != nil {
			t.Fatalf("seed artifact %s: %v", seed.objectKey, err)
		}
	}

	var backupID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, method_id, state, origin, bucket, object_key,
		                     size_bytes, started_at, completed_at, expires_at)
		VALUES ($1, $2, 'dump', $3, $4, $5, $6, $7, $8, $8, $9)
		RETURNING id`,
		metadb.DefaultTenantID, seed.instanceID, seed.state, seed.origin, bucket, seed.objectKey,
		seed.sizeBytes, seed.completedAt, seed.expiresAt).Scan(&backupID); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	if seed.verification != "" {
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO verifications (tenant_id, backup_id, status, started_at, completed_at)
			VALUES ($1, $2, $3, $4, $4)`,
			metadb.DefaultTenantID, backupID, seed.verification, seed.completedAt); err != nil {
			t.Fatalf("seed verification: %v", err)
		}
	}
	return backupID
}

// seedPendingVerifyJob creates the row that exists between a backup succeeding and its verification
// starting — a job naming the backup, with no verifications row yet.
func (h *harness) seedPendingVerifyJob(t *testing.T, ctx context.Context, backupID string) {
	t.Helper()

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO jobs (tenant_id, instance_id, kind, state, payload)
		VALUES ($1, $2, 'verify', 'pending', jsonb_build_object('backup_id', $3::text))`,
		metadb.DefaultTenantID, h.instance, backupID); err != nil {
		t.Fatalf("seed pending verify job: %v", err)
	}
}

func (h *harness) seedSecondInstance(t *testing.T, ctx context.Context, name string) string {
	t.Helper()

	var environmentID string
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM environments WHERE tenant_id = $1 LIMIT 1`, metadb.DefaultTenantID).
		Scan(&environmentID); err != nil {
		t.Fatalf("find environment: %v", err)
	}

	var instanceID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, $3, $4, 'db2.example.internal', 5432) RETURNING id`,
		metadb.DefaultTenantID, environmentID, name, engine).Scan(&instanceID); err != nil {
		t.Fatalf("seed second instance: %v", err)
	}
	return instanceID
}

func (h *harness) backupState(t *testing.T, ctx context.Context, backupID string) string {
	t.Helper()

	var state string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM backups WHERE id = $1`, backupID).Scan(&state); err != nil {
		t.Fatalf("read backup %s: %v", backupID, err)
	}
	return state
}

func (h *harness) expiresAt(t *testing.T, ctx context.Context, backupID string) *time.Time {
	t.Helper()

	var expires *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT expires_at FROM backups WHERE id = $1`, backupID).Scan(&expires); err != nil {
		t.Fatalf("read expiry of %s: %v", backupID, err)
	}
	return expires
}

func (h *harness) requireObjectPresent(t *testing.T, ctx context.Context, key string) {
	t.Helper()

	if _, err := h.store.Stat(ctx, key); err != nil {
		t.Fatalf("object %s should still exist: %v", key, err)
	}
}

func (h *harness) requireObjectAbsent(t *testing.T, ctx context.Context, key string) {
	t.Helper()

	_, err := h.store.Stat(ctx, key)
	if err == nil {
		t.Fatalf("object %s is still in the bucket; the row may say it went, but the bytes did not", key)
	}
	if !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("stat %s: %v", key, err)
	}
}

func idsOf(candidates []*fwv1.RetentionCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.GetBackupId())
	}
	return out
}
