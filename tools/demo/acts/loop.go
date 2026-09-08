package acts

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// actLoop is act 3: the loop, live.
//
// Everything before this is a fixture and says so. From here on nothing is seeded: a real backup of
// a real database by the engine's own native tooling, then a throwaway container of the matching
// engine, the artifact restored into it, the row counts compared against the manifest captured when
// the backup was taken, and the container destroyed on every path out.
func actLoop(ctx context.Context, n *Narrator, c *client, s *seeded) (backupRow, error) {
	n.Act(3, "The loop, live",
		"A real backup, and then a real verification. Nothing from here on is seeded.")

	instanceID := s.instances[liveInstance]
	if instanceID == "" {
		return backupRow{}, fmt.Errorf("the live instance %q is not in the seeded estate", liveInstance)
	}

	n.Step("backing up %s — BACKUP DATABASE, the engine's own tooling, orchestrated not reimplemented", liveInstance)
	var started struct {
		BackupID string `json:"backup_id"`
		JobID    string `json:"job_id"`
	}
	if err := c.post(ctx, "/api/v1/instances/"+instanceID+"/backups", map[string]any{
		"instance_id": instanceID,
	}, &started); err != nil {
		return backupRow{}, fmt.Errorf("run a backup of %s: %w", liveInstance, err)
	}

	backup, err := awaitBackup(ctx, n, c, started.BackupID, 10*time.Minute)
	if err != nil {
		return backupRow{}, err
	}
	if state := trimEnum("BACKUP_STATE_", backup.State); state != "SUCCEEDED" {
		return backupRow{}, fmt.Errorf("the backup of %s is %s: %s", liveInstance, state, backup.ErrorMessage)
	}
	if backup.Artifact == nil || backup.Artifact.Key == "" {
		return backupRow{}, fmt.Errorf("the backup of %s succeeded and recorded no artifact", liveInstance)
	}
	if backup.Checksum == nil || backup.Checksum.Value == "" {
		return backupRow{}, fmt.Errorf("the backup of %s recorded no checksum, so nothing could ever "+
			"tell a corrupted artifact from a bad restore", liveInstance)
	}
	n.Step("artifact %s (%s), %s", backup.Artifact.Key, humanBytes(backup.SizeBytes),
		strings.ToLower(trimEnum("CHECKSUM_ALGORITHM_", backup.Checksum.Algorithm))+" recorded with it")

	n.Say("")
	n.Say("A backup that has never been restored is a hypothesis. So:")

	verification, err := verifyNow(ctx, n, c, backup.ID)
	if err != nil {
		return backupRow{}, err
	}
	if status := trimEnum("VERIFICATION_STATUS_", verification.Status); status != "VERIFIED" {
		return backupRow{}, fmt.Errorf("the verification of a backup taken thirty seconds ago is %s: %s\n%s",
			status, verification.Report, checkLines(verification))
	}

	for _, check := range verification.Checks {
		n.Step("%s — %s", strings.ToLower(trimEnum("VERIFICATION_CHECK_", check.Check)), check.Message)
	}
	n.Say("")
	n.Say("VERIFIED. The artifact was restored into a container that no longer exists, and what " +
		"arrived matched the manifest captured when the backup was taken.")
	n.Beat()
	return backup, nil
}

// actCorruption is act 4, and it is the climax.
//
// The green result in act 3 is only worth anything because this one is red. A verification that has
// only ever been shown to pass is indistinguishable from one that always passes, which is worse
// than no verification at all because it manufactures confidence.
func actCorruption(ctx context.Context, cfg Config, n *Narrator, c *client, backup backupRow) error {
	n.Act(4, "Break it on purpose",
		"The same backup, the same verification, and one byte of the artifact changed where it "+
			"actually lives.")

	n.Step("overwriting one byte in the middle of %s/%s", backup.Artifact.Bucket, backup.Artifact.Key)
	size, err := corruptInPlace(ctx, cfg, backup.Artifact.Bucket, backup.Artifact.Key)
	if err != nil {
		return err
	}
	n.Step("%s written back, same length, checksum on the row untouched", humanBytes(strconv.FormatInt(size, 10)))
	n.Say("")
	n.Say("Nothing about the backup record changed. Fleetward still believes this artifact is %s bytes "+
		"with that checksum. The bytes in the bucket disagree, and that is exactly what bit rot and "+
		"a half-finished upload look like.", backup.SizeBytes)

	verification, err := verifyNow(ctx, n, c, backup.ID)
	if err != nil {
		return err
	}
	status := trimEnum("VERIFICATION_STATUS_", verification.Status)
	if status != "FAILED" {
		return fmt.Errorf("verification of a deliberately corrupted artifact came back %s, want FAILED.\n"+
			"FAILED is reserved for evidence about the artifact; %s would mean Fleetward could not "+
			"tell, which is a different answer and this is not it.\n%s",
			status, status, verification.Report)
	}

	n.Say("")
	n.Say("FAILED. Not INCONCLUSIVE — and the difference is a decision this product made on purpose.")
	n.Say("FAILED means evidence about the artifact: we can tell, and it is bad. Everything else — " +
		"a sandbox that never started, a plugin that died, a timeout — is INCONCLUSIVE, because " +
		"\"we could not tell\" and \"we can tell, and it is bad\" are different answers. A system " +
		"that reports infrastructure trouble as data loss gets muted, and a muted alert is the same " +
		"as no alert.")

	// The two-part status, read back exactly as the estate view reads it: the backup still
	// succeeded, and the verification says the artifact is not what it claims to be.
	after, err := getBackup(ctx, c, backup.ID)
	if err != nil {
		return err
	}
	if trimEnum("BACKUP_STATE_", after.State) != "SUCCEEDED" {
		return fmt.Errorf("the backup row is %s; corrupting the artifact must not rewrite the "+
			"history of the backup job, which did succeed", after.State)
	}
	if after.Verification == nil || trimEnum("VERIFICATION_STATUS_", after.Verification.Status) != "FAILED" {
		return fmt.Errorf("the estate view would not show this backup as failing verification, " +
			"which is the whole thing this act exists to demonstrate")
	}

	n.Say("")
	n.Table([]string{"INSTANCE", "BACKUP", "VERIFICATION"},
		[][]string{{liveInstance, "SUCCEEDED", "FAILED"}})
	n.Say("")
	n.Say("That is the two-part status, and it is why it is two parts. Every tool in this category "+
		"can tell you the left-hand column. The estate view at %s now shows this row as the "+
		"loudest thing on the screen — louder than an instance that has never been backed up at "+
		"all, because a backup nobody can restore is worse than a backup nobody took.", webURL())
	n.Say("")
	n.Say("The green result in act 3 is only worth anything because this one is red.")
	n.Beat()
	return nil
}

// verifyNow starts a verification and follows it, narrating so the demo does not look hung: a real
// restore pulls an image, starts a database and replays an artifact, and takes tens of seconds.
func verifyNow(ctx context.Context, n *Narrator, c *client, backupID string) (verificationRow, error) {
	var started struct {
		VerificationID string `json:"verification_id"`
	}
	if err := c.post(ctx, "/api/v1/backups/"+backupID+"/verify", map[string]any{}, &started); err != nil {
		return verificationRow{}, fmt.Errorf("start a verification of %s: %w", backupID, err)
	}
	n.Step("verification %s: provisioning a sandbox, restoring into it, counting what arrived", started.VerificationID)
	return awaitVerification(ctx, n, c, started.VerificationID, 20*time.Minute)
}

func awaitBackup(ctx context.Context, n *Narrator, c *client, backupID string, within time.Duration) (backupRow, error) {
	deadline := time.Now().Add(within)
	last := ""
	for {
		row, err := getBackup(ctx, c, backupID)
		if err != nil {
			return backupRow{}, err
		}
		state := trimEnum("BACKUP_STATE_", row.State)
		if state != last {
			n.Step("backup %s", strings.ToLower(state))
			last = state
		}
		switch state {
		case "SUCCEEDED", "FAILED", "CANCELED", "EXPIRED":
			return row, nil
		}
		if time.Now().After(deadline) {
			return backupRow{}, fmt.Errorf("the backup was still %s after %s", strings.ToLower(state), within)
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return backupRow{}, err
		}
	}
}

func awaitVerification(
	ctx context.Context, n *Narrator, c *client, verificationID string, within time.Duration,
) (verificationRow, error) {
	deadline := time.Now().Add(within)
	last := ""
	for {
		var resp struct {
			Verification verificationRow `json:"verification"`
		}
		if err := c.get(ctx, "/api/v1/verifications/"+verificationID, nil, &resp); err != nil {
			return verificationRow{}, fmt.Errorf("read verification %s: %w", verificationID, err)
		}
		// A verification that has a row and no status yet is queued behind its job. UNSPECIFIED is
		// what the wire says and "pending" is what is happening, so the narration says the latter.
		status := trimEnum("VERIFICATION_STATUS_", resp.Verification.Status)
		if status == "UNSPECIFIED" || status == "UNKNOWN" {
			status = "PENDING"
		}
		if status != last {
			n.Step("verification %s", strings.ToLower(status))
			last = status
		}
		switch status {
		case "VERIFIED", "FAILED", "INCONCLUSIVE":
			return resp.Verification, nil
		}
		if time.Now().After(deadline) {
			return verificationRow{}, fmt.Errorf("the verification was still %s after %s",
				strings.ToLower(status), within)
		}
		if err := wait(ctx, 3*time.Second); err != nil {
			return verificationRow{}, err
		}
	}
}

func getBackup(ctx context.Context, c *client, backupID string) (backupRow, error) {
	var resp struct {
		Backup backupRow `json:"backup"`
	}
	if err := c.get(ctx, "/api/v1/backups/"+backupID, nil, &resp); err != nil {
		return backupRow{}, fmt.Errorf("read backup %s: %w", backupID, err)
	}
	return resp.Backup, nil
}

func checkLines(v verificationRow) string {
	var b strings.Builder
	for _, check := range v.Checks {
		fmt.Fprintf(&b, "  %s: %s\n", trimEnum("VERIFICATION_CHECK_", check.Check), check.Message)
	}
	return b.String()
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// humanBytes renders a protobuf JSON integer, which arrives as a string, at the scale an operator
// reads it in.
func humanBytes(raw string) string {
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size <= 0 {
		return raw + " B"
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
