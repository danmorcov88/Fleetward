package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newBackupRetentionCommand shows what retention would delete, without deleting anything.
//
// It exists because retention is the only thing Fleetward does that cannot be undone, and because
// there is no job row behind a sweep to read afterwards (ADR-0030). An operator deciding whether to
// enable retention, or wondering why an artifact is still there, is answered here or not at all.
func newBackupRetentionCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var instanceName string

	cmd := &cobra.Command{
		Use:   "retention",
		Short: "What the next retention sweep would delete, and what it would keep",
		Long: "Reports the artifacts whose retention has run out, and the ones that have run out\n" +
			"and are kept anyway. Nothing is deleted by running this.\n\n" +
			"Only backups Fleetward took appear here. A backup Fleetward merely observed is\n" +
			"somebody else's file and is never deleted, whatever any policy says.\n\n" +
			"A backup with no expiry — one taken manually, or taken before this version of\n" +
			"Fleetward existed — is never deleted either. Retention applies from the moment a\n" +
			"schedule's `--retention-days` was in force when the backup ran, and is stamped on the\n" +
			"backup then rather than recalculated later.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout, *token)
			query := url.Values{}
			if instanceName != "" {
				inst, err := resolveInstance(ctx, c, instanceName)
				if err != nil {
					return err
				}
				query.Set("instance_id", inst.ID)
			}

			var resp retentionPreviewResponse
			if err := c.get(ctx, "/api/v1/backup-retention", query, &resp); err != nil {
				return err
			}
			printRetentionPreview(cmd.OutOrStdout(), &resp)
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier")
	return cmd
}

func printRetentionPreview(out io.Writer, resp *retentionPreviewResponse) {
	printRetentionPolicy(out, resp.Policy)

	if len(resp.Expiring) == 0 && len(resp.Protected) == 0 && len(resp.PendingDeletion) == 0 {
		fmt.Fprintln(out, "\nnothing has outlived its retention")
		return
	}

	if len(resp.Expiring) > 0 {
		fmt.Fprintf(out, "\nWOULD BE DELETED (%d, %s)\n",
			len(resp.Expiring), humanBytes(parseInt64(resp.ReclaimableBytes)))
		printRetentionRows(out, resp.Expiring, false)
	}

	if len(resp.PendingDeletion) > 0 {
		// This list is normally empty. It fills when a control plane stopped between marking a
		// backup expired and deleting its object, and the next sweep drains it — so a row here is
		// information rather than a problem, and saying so stops it reading as one.
		fmt.Fprintf(out, "\nALREADY EXPIRED, ARTIFACT STILL PRESENT (%d)\n", len(resp.PendingDeletion))
		fmt.Fprintln(out, "a sweep was interrupted between marking these and deleting them; the next one finishes")
		printRetentionRows(out, resp.PendingDeletion, false)
	}

	if len(resp.Protected) > 0 {
		fmt.Fprintf(out, "\nPAST ITS RETENTION AND KEPT ANYWAY (%d)\n", len(resp.Protected))
		printRetentionRows(out, resp.Protected, true)
	}
}

// printRetentionPolicy renders the limits, and says plainly when the sweep is off.
//
// A preview that listed twelve artifacts as "would be deleted" without mentioning that nothing is
// running would be actively misleading in the one place this product cannot afford to be.
func printRetentionPolicy(out io.Writer, p *retentionPolicyRow) {
	if p == nil {
		return
	}
	if !p.Enabled {
		fmt.Fprintln(out, "retention is DISABLED; nothing is being deleted. "+
			"Set FLEETWARD_RETENTION_ENABLED=true to act on what follows.")
		return
	}
	fmt.Fprintf(out, "retention runs every %s, keeps at least %d recent backup(s) per instance, "+
		"and deletes at most %d artifacts per sweep\n",
		humanDuration(p.Interval), p.MinKeep, p.MaxPerSweep)
}

func printRetentionRows(out io.Writer, rows []retentionCandidate, withReason bool) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "INSTANCE\tBACKUP\tFINISHED (UTC)\tEXPIRED (UTC)\tSIZE"
	if withReason {
		header += "\tWHY IT STAYS"
	}
	fmt.Fprintln(w, header)

	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s",
			r.InstanceName, r.BackupID,
			formatRetentionTime(r.CompletedAt), formatRetentionTime(r.ExpiresAt),
			humanBytes(parseInt64(r.SizeBytes)))
		if withReason {
			fmt.Fprintf(w, "\t%s", r.ProtectedReason)
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
}

func formatRetentionTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// humanDuration renders a protobuf duration, which arrives over the gateway as "3600s".
func humanDuration(raw string) string {
	if raw == "" {
		return "—"
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return raw
	}
	return d.String()
}
