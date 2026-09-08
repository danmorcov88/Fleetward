package acts

import (
	"context"
	"fmt"
	"strings"
)

// actAdherence is act 2, and it is the product thesis in one command.
//
// Declare what should be true, detect what actually is, show the gap. Everything else Fleetward
// does is machinery in service of this table.
func actAdherence(ctx context.Context, n *Narrator, c *client, s *seeded) error {
	n.Act(2, "Declare, detect, gap",
		"What was declared, what was detected, and the difference. `fleetward-cli backup adherence` "+
			"asks the estate one question, and the answer is computed on read rather than stored.")

	rows, err := adherence(ctx, c)
	if err != nil {
		return err
	}
	rows = mine(rows, s)

	table := make([][]string, 0, len(rows))
	states := map[string]int{}
	var caveated []adherenceRow
	for _, row := range rows {
		state := trimEnum("ADHERENCE_STATE_", row.State)
		states[state]++

		detail := "—"
		switch {
		case row.SatisfiedBy != nil && row.SatisfiedBy.CompletedAt != nil:
			detail = "backup at " + row.SatisfiedBy.CompletedAt.UTC().Format("2006-01-02 15:04Z")
		case row.LatestBackup != nil && row.LatestBackup.CompletedAt != nil:
			detail = "last one " + row.LatestBackup.CompletedAt.UTC().Format("2006-01-02 15:04Z")
		case row.LatestBackup == nil:
			detail = "no backup has ever been recorded"
		}
		if len(row.Caveats) > 0 {
			detail += "  (see below)"
			caveated = append(caveated, row)
		}
		table = append(table, []string{row.InstanceName, row.ExpectedCron, state, detail})
	}
	n.Table([]string{"INSTANCE", "EXPECTED", "STATE", "EVIDENCE"}, table)

	// Caveats are sentences rather than cells, and they are the most interesting thing on the
	// screen: each one is a fact the plugin declared about its own source, not something core
	// inferred. An answer resting on a backup Fleetward took has none of them and says nothing.
	for _, row := range caveated {
		n.Say("")
		n.Say("%s — what weakens this answer:", row.InstanceName)
		for _, caveat := range row.Caveats {
			n.Say("  · %s", strings.TrimSpace(caveat))
		}
	}

	n.Say("")
	n.Say("Four different answers, and the difference between them is the point.")
	n.Say("ADHERENT — a backup Fleetward can account for landed inside the declared window.")
	n.Say("MISSED — the window closed and nothing landed in it. This is the gap.")
	n.Say("UNPROVEN — something landed, and the evidence cannot say whether it worked. A file that " +
		"arrived is not a backup that worked, and rendering those as the same green tick is the " +
		"false confidence this product exists to eliminate.")
	n.Say("NOT_DECLARED — nothing was declared, so nothing can be missing. Fleetward reports that " +
		"rather than guessing a schedule from the rhythm it happens to observe.")

	// The three the fixture exists to produce. Asserting on the counts rather than on the names
	// keeps this readable as a claim about the product rather than about the fixture.
	if states["MISSED"] < 1 {
		return fmt.Errorf("no instance is reported as behind its window, so the fixture's %q row "+
			"is not being detected", "pg-invoices-prod")
	}
	if states["UNPROVEN"] < 1 {
		return fmt.Errorf("no instance is reported as UNPROVEN, so evidence that cannot report an " +
			"outcome is being rounded up to success")
	}
	if states["ADHERENT"] < 6 {
		return fmt.Errorf("only %d instances are adherent, want at least 6: without the ordinary "+
			"case the exceptions do not read as exceptions", states["ADHERENT"])
	}
	n.Beat()
	return nil
}

func adherence(ctx context.Context, c *client) ([]adherenceRow, error) {
	var resp struct {
		Instances []adherenceRow `json:"instances"`
	}
	if err := c.get(ctx, "/api/v1/backup-adherence", nil, &resp); err != nil {
		return nil, fmt.Errorf("read backup adherence: %w", err)
	}
	return resp.Instances, nil
}

// mine narrows an estate-wide answer to the demo's own instances, so a stack somebody has already
// been using does not make the assertions ambiguous.
func mine(rows []adherenceRow, s *seeded) []adherenceRow {
	ids := map[string]bool{}
	for _, id := range s.instances {
		ids[id] = true
	}
	out := make([]adherenceRow, 0, len(s.instances))
	for _, row := range rows {
		if ids[row.InstanceID] {
			out = append(out, row)
		}
	}
	return out
}
