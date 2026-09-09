package acts

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// actAuthorization is act 5: who may do this, and where the answer is written down.
//
// Two callers, the same server, the same request, two answers — and both attempts recorded in a
// table the database itself refuses to let anybody edit.
func actAuthorization(ctx context.Context, n *Narrator, c *client, db *metadb, s *seeded) error {
	n.Act(5, "Who may do this",
		"A viewer refused, a dba allowed, and both attempts written somewhere neither of them can "+
			"erase.")

	instanceID := s.instances[liveInstance]
	viewer := c.as(s.viewerToken)
	dba := c.as(s.dbaToken)

	// A restore into a sandbox, refused. This is the request act 3 and act 4 both made.
	code, detail, err := viewer.status(ctx, http.MethodPost,
		"/api/v1/instances/"+instanceID+"/backups", map[string]any{"instance_id": instanceID})
	if err != nil {
		return err
	}
	if code != http.StatusForbidden {
		return fmt.Errorf("a viewer asking for a backup got %d, want 403: the whole authorization "+
			"spine is one policy table and a decorator, and this is the assertion it exists for", code)
	}
	n.Step("viewer  → POST /api/v1/instances/{id}/backups  → 403")
	n.Say("  %s", firstLine(detail))

	code, _, err = viewer.status(ctx, http.MethodGet, "/api/v1/audit", nil)
	if err != nil {
		return err
	}
	if code != http.StatusForbidden {
		return fmt.Errorf("a viewer reading the audit log got %d, want 403", code)
	}
	n.Step("viewer  → GET  /api/v1/audit                   → 403")

	// The same request from a credential that may make it, and it really runs.
	var started struct {
		BackupID string `json:"backup_id"`
	}
	if err := dba.post(ctx, "/api/v1/instances/"+instanceID+"/backups",
		map[string]any{"instance_id": instanceID}, &started); err != nil {
		return fmt.Errorf("a dba asking for a backup was refused, and should not have been: %w", err)
	}
	n.Step("dba     → POST /api/v1/instances/{id}/backups  → accepted, backup %s", started.BackupID)

	backup, err := awaitBackup(ctx, n, c, started.BackupID, 10*time.Minute)
	if err != nil {
		return err
	}
	if state := trimEnum("BACKUP_STATE_", backup.State); state != "SUCCEEDED" {
		return fmt.Errorf("the dba's backup is %s: %s", state, backup.ErrorMessage)
	}

	n.Say("")
	n.Say("Nothing about the request differed except the credential. The refusal is the server's, " +
		"decided from a policy table keyed on the RPC, and no route reaches its handler without " +
		"passing through it — a method nobody has written a policy for is denied to everybody, " +
		"administrators included.")

	// Both attempts, in the log, including the one that was refused. A 403 that names somebody is
	// the most interesting row in an audit log.
	entries, err := auditLog(ctx, c, url.Values{"resource_id": {instanceID}, "page_size": {"20"}})
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(entries))
	refusals, allowed := 0, 0
	for _, e := range entries {
		outcome := "allowed"
		if !e.Succeeded {
			outcome = "REFUSED"
			refusals++
		}
		if e.Succeeded && strings.HasPrefix(e.Actor, "demo-dba") {
			allowed++
		}
		rows = append(rows, []string{
			e.OccurredAt.UTC().Format("15:04:05Z"), e.Actor, e.Action, outcome,
		})
		if len(rows) >= 12 {
			break
		}
	}
	n.Say("")
	n.Table([]string{"WHEN", "ACTOR", "ACTION", "OUTCOME"}, rows)

	if refusals < 1 {
		return fmt.Errorf("the audit log records no refusal, so a 403 leaves no evidence that " +
			"somebody real reached for something they may not have")
	}
	if allowed < 1 {
		return fmt.Errorf("the audit log records nothing the dba was allowed to do, so only " +
			"failures are being written")
	}

	// The log is append-only at the database level rather than by convention, and the cheapest way
	// to show that is to try.
	if err := db.demonstrateAppendOnly(ctx, n); err != nil {
		return err
	}

	n.Say("")
	n.Say("And the actor is not always a person. The retention sweep runs with no user row and no " +
		"credential, so \"who deleted this artifact\" has an answer:")
	swept, err := waitForRetentionAudit(ctx, n, c, 3*time.Minute)
	if err != nil {
		return err
	}
	sweptRows := make([][]string, 0, len(swept))
	for _, e := range swept {
		sweptRows = append(sweptRows, []string{
			e.OccurredAt.UTC().Format("15:04:05Z"), e.Actor, e.Action, e.ResourceID,
		})
		if len(sweptRows) >= 5 {
			break
		}
	}
	n.Table([]string{"WHEN", "ACTOR", "ACTION", "BACKUP"}, sweptRows)
	n.Beat()
	return nil
}

// actRetention is act 6: what retention refuses to delete, and why.
//
// The interesting row is not the artifact that goes. It is the one that has outlived its retention
// by a fortnight and stays anyway, because it is the last backup of that instance anybody has ever
// proven restorable (ADR-0032).
func actRetention(ctx context.Context, n *Narrator, c *client, db *metadb, s *seeded) error {
	n.Act(6, "What it refuses to delete",
		"Retention is the only thing this product does that destroys something, so it can be asked "+
			"what it would do before it does any of it.")

	instanceID := s.instances["mssql-archive-prod"]
	if instanceID == "" {
		return fmt.Errorf("the fixture's unverifiable instance is missing from the estate")
	}

	// One more expiry, stamped now, so the answer has a row in both columns: the development stack
	// sweeps every minute and has usually already removed the ones act 1 stamped.
	if err := db.stampUnprotectedExpiry(ctx, instanceID); err != nil {
		return err
	}

	preview, err := retentionPreview(ctx, c, instanceID)
	if err != nil {
		return err
	}

	n.Say("mssql-archive-prod has backed up successfully every night for a fortnight, and only one " +
		"of those backups has ever been proven restorable. Its retention is fourteen days.")
	n.Say("")
	n.Seeded("some of its expiries were stamped into the past so this act has something to show " +
		"without waiting a fortnight; nothing else about the rows was touched")
	n.Say("")

	if len(preview.Protected) == 0 {
		return fmt.Errorf("nothing is protected on %s, so the retention floor is not doing the one "+
			"thing it exists to do", "mssql-archive-prod")
	}
	rows := make([][]string, 0, len(preview.Protected)+len(preview.Expiring))
	for _, p := range preview.Protected {
		rows = append(rows, []string{"KEPT", shortID(p.BackupID), expiredFor(p), p.ProtectedReason})
	}
	for _, e := range preview.Expiring {
		rows = append(rows, []string{"DELETES", shortID(e.BackupID), expiredFor(e), "nothing protects it"})
	}
	n.Table([]string{"NEXT SWEEP", "BACKUP", "EXPIRY", "WHY"}, rows)

	proven := false
	for _, p := range preview.Protected {
		if strings.Contains(p.ProtectedReason, "proven restorable") {
			proven = true
		}
	}
	if !proven {
		return fmt.Errorf("no protected backup is kept for being the last one proven restorable, " +
			"which is the rule that makes the floor worth having")
	}

	n.Say("")
	n.Say("Keeping only the newest would have kept a backup known to be unrestorable and deleted " +
		"the last one proven good. Verification decides the floor; it never decides eligibility.")
	n.Say("")
	n.Say("And the row is never deleted, only its bytes. What is left is the record of what once " +
		"existed, which is what made the audit rows in act 5 possible.")
	n.Beat()
	return nil
}

// demonstrateAppendOnly tries to delete an audit row and requires the database to refuse.
//
// Through the metadata connection rather than the API, deliberately: the API has no endpoint that
// could do this, so the only way to show that the guarantee is the database's rather than the
// application's is to attempt it where an application would not be in the way.
func (m *metadb) demonstrateAppendOnly(ctx context.Context, n *Narrator) error {
	_, err := m.pool.Exec(ctx, `DELETE FROM audit_log WHERE tenant_id = $1`, m.tenant)
	if err == nil {
		return fmt.Errorf("a direct DELETE against audit_log succeeded; the append-only trigger is " +
			"not doing its job, and every record in that table is worth less than it appears")
	}
	n.Say("")
	n.Step("DELETE FROM audit_log — refused by the database itself")
	n.Say("  %s", firstLine(err.Error()))
	return nil
}

// waitForRetentionAudit polls until the sweep has recorded something, which it does when it deletes.
func waitForRetentionAudit(ctx context.Context, n *Narrator, c *client, within time.Duration) ([]auditRow, error) {
	deadline := time.Now().Add(within)
	announced := false
	for {
		entries, err := auditLog(ctx, c, url.Values{"actor": {"system:retention"}, "page_size": {"10"}})
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			return entries, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the retention sweep recorded nothing within %s, so either it is "+
				"not running or it found nothing whose expiry had passed", within)
		}
		if !announced {
			n.Step("waiting for a retention sweep — it runs on the scheduler's tick, holding no lease")
			announced = true
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return nil, err
		}
	}
}

func auditLog(ctx context.Context, c *client, query url.Values) ([]auditRow, error) {
	var resp struct {
		Entries []auditRow `json:"entries"`
	}
	if err := c.get(ctx, "/api/v1/audit", query, &resp); err != nil {
		return nil, fmt.Errorf("read the audit log: %w", err)
	}
	return resp.Entries, nil
}

type retentionPreviewResponse struct {
	Policy struct {
		Enabled     bool   `json:"enabled"`
		Interval    string `json:"interval"`
		MinKeep     int32  `json:"min_keep"`
		MaxPerSweep int32  `json:"max_per_sweep"`
	} `json:"policy"`
	Expiring         []retentionCandidateRow `json:"expiring"`
	Protected        []retentionCandidateRow `json:"protected"`
	PendingDeletion  []retentionCandidateRow `json:"pending_deletion"`
	ReclaimableBytes string                  `json:"reclaimable_bytes"`
}

func retentionPreview(ctx context.Context, c *client, instanceID string) (retentionPreviewResponse, error) {
	var out retentionPreviewResponse
	if err := c.get(ctx, "/api/v1/backup-retention", url.Values{"instance_id": {instanceID}}, &out); err != nil {
		return retentionPreviewResponse{}, fmt.Errorf("preview retention: %w", err)
	}
	return out, nil
}

func expiredFor(c retentionCandidateRow) string {
	if c.ExpiresAt == nil {
		return "never"
	}
	d := time.Since(*c.ExpiresAt)
	if d < 0 {
		return "in " + d.Abs().Round(time.Hour).String()
	}
	switch days := int(d.Hours() / 24); days {
	case 0:
		return "passed today"
	case 1:
		return "passed 1 day ago"
	default:
		return fmt.Sprintf("passed %d days ago", days)
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
