package acts

import (
	"context"
	"fmt"
)

// Run performs every act in order, and is the whole of the demo and the whole of the end-to-end
// test (ADR-0037).
//
// Every act asserts. There is no mode in which the demo prints something without checking it,
// because the two entry points differ only in whether a person is watching — and a demo that
// narrates a result it has not verified is exactly how a demo starts lying.
func Run(ctx context.Context, cfg Config) (err error) {
	n := NewNarrator(cfg)
	n.Say("Fleetward — the whole loop, on a real stack.")
	n.Say("")
	n.Say("Most backup tooling tells you the job finished. This restores the artifact into a " +
		"throwaway container, counts the rows against a manifest captured at backup time, and " +
		"destroys the container. Then it corrupts the artifact on purpose and shows the same check " +
		"going red — because a verification that has only ever been seen to pass is " +
		"indistinguishable from one that always passes.")

	if err := actStack(ctx, cfg, n); err != nil {
		return fmt.Errorf("act 0, the stack: %w", err)
	}
	// The teardown runs whatever happens after the stack is up, including on a failure part way
	// through: a demo that leaves eleven containers behind on the way out is one nobody runs twice.
	defer ComposeDown(context.WithoutCancel(ctx), cfg, n)

	db, err := openMetadb(ctx, cfg)
	if err != nil {
		return fmt.Errorf("act 1, the estate: %w", err)
	}
	defer db.Close()

	c := newClient(cfg.ServerURL, cfg.Token)

	seed, err := actEstate(ctx, cfg, n, c, db)
	if err != nil {
		return fmt.Errorf("act 1, the estate: %w", err)
	}
	if err := actAdherence(ctx, n, c, seed); err != nil {
		return fmt.Errorf("act 2, declare-detect-gap: %w", err)
	}

	backup, err := actLoop(ctx, n, c, seed)
	if err != nil {
		return fmt.Errorf("act 3, the loop: %w", err)
	}

	// Act 7's delivery, configured here rather than in act 7, because a notification goes out on the
	// transition into `firing` and act 4 is what causes that transition. A destination created
	// afterwards would correctly be told nothing, and act 7 would have nothing to show — which is
	// the product behaving properly and would read as the demo being broken.
	//
	// Silent, because it is setup rather than a beat. Act 7 says out loud that it happened here and
	// why, so nothing is passed off as having arrived unprompted when it did not.
	delivery := startAlertDelivery(ctx, c)
	defer delivery.Close(context.WithoutCancel(ctx), c)

	if err := actCorruption(ctx, cfg, n, c, backup); err != nil {
		return fmt.Errorf("act 4, breaking it on purpose: %w", err)
	}
	if err := actAuthorization(ctx, n, c, db, seed); err != nil {
		return fmt.Errorf("act 5, who may do this: %w", err)
	}
	if err := actRetention(ctx, n, c, db, seed); err != nil {
		return fmt.Errorf("act 6, what it refuses to delete: %w", err)
	}
	// The backup act 4 corrupted, which is what act 7's alert is about. The two acts are far apart
	// on the screen and one row apart in the database, which is the point: the alert names the
	// artifact rather than restating the beat.
	if err := actAlerts(ctx, n, c, backup, delivery); err != nil {
		return fmt.Errorf("act 7, the alert: %w", err)
	}

	n.Act(8, "That was the product",
		"Declare what should be true, detect what actually is, and show the gap — for backups that "+
			"Fleetward took and for backups it merely watched.")
	n.Say("Every number on the screen came from this stack. The six weeks of history were seeded " +
		"so the estate had something to say; the backup, the verification, the corrupted artifact, " +
		"the refusal, the audit rows, the retention answer and the alert that reached a webhook " +
		"were all live.")
	n.Say("")
	n.Say("docs/demo.md says exactly what is seeded. docs/dev/STATUS.md says exactly what is not " +
		"built.")
	return nil
}
