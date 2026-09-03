package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// pollInterval is how often `backup run --wait` asks the control plane whether the backup has
// finished. The RPC that starts a backup returns immediately — a backup takes minutes to hours and
// an HTTP request must not — so progress is observed by polling rather than streamed.
const pollInterval = 2 * time.Second

// newBackupCommand builds the `backup` group.
func newBackupCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take and inspect backups",
		Long: "Backups are taken by the engine's own native tooling, orchestrated by Fleetward.\n" +
			"Every backup Fleetward takes carries a manifest of what the source contained when it\n" +
			"was taken, which is what makes the verification in `backup verify` mean something.\n\n" +
			"Fleetward also reports on backups it did not take: `backup history` and\n" +
			"`backup adherence` cover an estate whose backups are already being taken by something\n" +
			"else, without changing anything about it — and `backup retention` shows what Fleetward\n" +
			"would delete of its own, before it deletes any of it.",
	}
	cmd.AddCommand(
		newBackupRunCommand(serverURL, timeout, token),
		newBackupShowCommand(serverURL, timeout, token),
		newBackupVerifyCommand(serverURL, timeout, token),
		newBackupHistoryCommand(serverURL, timeout, token),
		newBackupObserveCommand(serverURL, timeout, token),
		newBackupAdherenceCommand(serverURL, timeout, token),
		newBackupRetentionCommand(serverURL, timeout, token),
	)
	return cmd
}

func newBackupRunCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		instanceName string
		methodID     string
		databases    []string
		options      []string
		verify       bool
		wait         bool
		waitTimeout  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a backup of one instance",
		Long: "Starts a backup and, unless --wait=false, follows it until it finishes.\n\n" +
			"The method comes from the plugin serving the instance; leaving --method unset uses\n" +
			"whichever method that plugin marks as its default.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsedOptions, err := parseKeyValues(options, "option")
			if err != nil {
				return err
			}

			c := newClient(*serverURL, *timeout, *token)

			startCtx, cancelStart := context.WithTimeout(cmd.Context(), *timeout)
			defer cancelStart()

			inst, err := resolveInstance(startCtx, c, instanceName)
			if err != nil {
				return err
			}

			body := map[string]any{"instance_id": inst.ID}
			if verify {
				body["verify_on_completion"] = true
			}
			if methodID != "" {
				body["method_id"] = methodID
			}
			if len(databases) > 0 {
				body["databases"] = databases
			}
			if len(parsedOptions) > 0 {
				body["options"] = parsedOptions
			}

			var started struct {
				BackupID string `json:"backup_id"`
				JobID    string `json:"job_id"`
			}
			if err := c.post(startCtx, "/api/v1/instances/"+inst.ID+"/backups", body, &started); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "backup %s started on %s\n", started.BackupID, inst.Name)
			if !wait {
				fmt.Fprintf(out, "follow it with: fleetward-cli backup show %s\n", started.BackupID)
				return nil
			}

			// The wait deadline is its own, not the per-request one: a backup legitimately takes far
			// longer than the control plane is allowed to take answering a question about it.
			waitCtx, cancelWait := context.WithTimeout(cmd.Context(), waitTimeout)
			defer cancelWait()

			result, err := followBackup(waitCtx, c, started.BackupID, func(state string) {
				fmt.Fprintf(out, "  %s\n", strings.ToLower(state))
			})
			if err != nil {
				return err
			}

			printBackup(out, result)
			if verify && result.Backup.State == "BACKUP_STATE_SUCCEEDED" {
				// The verification is a separate job with its own row, so the CLI points at it
				// rather than following it: a backup and its proof are two facts, and conflating
				// them is exactly what the two-part status in the UI exists to prevent.
				fmt.Fprintf(out, "\nverification started; read its outcome with: "+
					"fleetward-cli backup show %s\n", started.BackupID)
			}
			if result.Backup.State != "BACKUP_STATE_SUCCEEDED" {
				return fmt.Errorf("backup %s", strings.ToLower(trimEnum("BACKUP_STATE_", result.Backup.State)))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier (required)")
	cmd.Flags().StringVar(&methodID, "method", "", "backup method; empty uses the plugin's default")
	cmd.Flags().StringSliceVar(&databases, "database", nil,
		"database to back up; repeatable, empty means the connection's own database")
	cmd.Flags().StringArrayVar(&options, "option", nil, "method option as key=value; repeatable")
	cmd.Flags().BoolVar(&verify, "verify", false,
		"verify the backup as soon as it succeeds; follow it with `backup verify --backup <id>`")
	cmd.Flags().BoolVar(&wait, "wait", true, "follow the backup until it finishes")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 2*time.Hour, "how long to follow a running backup")
	_ = cmd.MarkFlagRequired("instance")

	return cmd
}

func newBackupShowCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var showManifest bool

	cmd := &cobra.Command{
		Use:   "show <backup-id>",
		Short: "Show one backup and its manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout, *token)
			var resp backupResponse
			if err := c.get(ctx, "/api/v1/backups/"+args[0], nil, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			printBackup(out, &resp)
			if showManifest {
				printManifest(out, &resp)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&showManifest, "manifest", false, "list every object the manifest recorded")
	return cmd
}

// followBackup polls until the backup reaches a terminal state, reporting each state change.
func followBackup(ctx context.Context, c *client, backupID string, onState func(state string)) (*backupResponse, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	lastState := ""
	for {
		var resp backupResponse
		if err := c.get(ctx, "/api/v1/backups/"+backupID, nil, &resp); err != nil {
			return nil, err
		}

		if resp.Backup.State != lastState {
			lastState = resp.Backup.State
			onState(trimEnum("BACKUP_STATE_", resp.Backup.State))
		}
		if isTerminalBackupState(resp.Backup.State) {
			return &resp, nil
		}

		select {
		case <-ctx.Done():
			// The backup itself is unaffected: it runs in the control plane, not here.
			return nil, fmt.Errorf("stopped waiting for backup %s; it is still running — "+
				"check it with: fleetward-cli backup show %s", backupID, backupID)
		case <-ticker.C:
		}
	}
}

func isTerminalBackupState(state string) bool {
	switch state {
	case "BACKUP_STATE_SUCCEEDED", "BACKUP_STATE_FAILED", "BACKUP_STATE_CANCELED",
		"BACKUP_STATE_EXPIRED", "BACKUP_STATE_UNKNOWN":
		return true
	default:
		return false
	}
}

func printBackup(out io.Writer, resp *backupResponse) {
	b := resp.Backup
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	fmt.Fprintf(w, "id\t%s\n", b.ID)
	fmt.Fprintf(w, "origin\t%s\n", strings.ToLower(trimEnum("BACKUP_ORIGIN_", b.Origin)))
	fmt.Fprintf(w, "state\t%s\n", trimEnum("BACKUP_STATE_", b.State))
	fmt.Fprintf(w, "method\t%s\n", b.MethodID)
	if b.ExternalLocation != "" {
		fmt.Fprintf(w, "location\t%s\n", b.ExternalLocation)
	}
	if b.Artifact != nil && b.Artifact.Key != "" {
		fmt.Fprintf(w, "artifact\t%s/%s\n", b.Artifact.Bucket, b.Artifact.Key)
	}
	if size := parseInt64(b.SizeBytes); size > 0 {
		fmt.Fprintf(w, "size\t%s\n", humanBytes(size))
	}
	if b.Checksum != nil && b.Checksum.Value != "" {
		fmt.Fprintf(w, "checksum\t%s %s\n", trimEnum("CHECKSUM_ALGORITHM_", b.Checksum.Algorithm), b.Checksum.Value)
	}
	if b.StartedAt != nil {
		fmt.Fprintf(w, "started\t%s\n", b.StartedAt.Format(time.RFC3339))
	}
	if b.Duration != "" {
		fmt.Fprintf(w, "duration\t%s\n", b.Duration)
	}
	if b.ConsistencyPoint != nil {
		fmt.Fprintf(w, "consistent to\t%s\n", b.ConsistencyPoint.Format(time.RFC3339))
	}
	if b.ErrorMessage != "" {
		fmt.Fprintf(w, "error\t%s\n", b.ErrorMessage)
	}

	// Printed even when there is none. "Not verified" is information an operator needs, and an
	// absent line reads as "fine" rather than as "unproven".
	verified := "never verified"
	if e := b.Evidence; e != nil {
		// An observed backup is not an unverified backup, and saying so is the whole point of
		// carrying an origin at all. What it can be checked against is stated here instead.
		verified = "not possible — Fleetward did not take this backup, so there is no manifest to verify it against"
		fmt.Fprintf(w, "evidence\t%s\n", e.SourceDescription)
		if !e.ReportsOutcome {
			fmt.Fprintf(w, "\t%s\n", "this source cannot say whether the backup completed successfully")
		}
		if e.CompletedAtIsApproximate {
			fmt.Fprintf(w, "\t%s\n", "the finish time may be out by up to an hour across a daylight-saving change")
		}
		if e.ObservedAt != nil {
			fmt.Fprintf(w, "observed\t%s\n", e.ObservedAt.UTC().Format(time.RFC3339))
		}
	}
	if b.Verification != nil {
		verified = trimEnum("VERIFICATION_STATUS_", b.Verification.Status)
		if verified == "UNSPECIFIED" {
			verified = "RUNNING"
		}
		verified += " (" + b.Verification.ID + ")"
	}
	fmt.Fprintf(w, "verification\t%s\n", verified)

	// The manifest summary is printed even without --manifest: it is the evidence that this backup
	// can be verified at all, and a backup without one can only ever be checked for "did it start".
	if m := resp.Manifest; m != nil && len(m.Entries) > 0 {
		sampled := ""
		if m.IsSampled {
			sampled = " (sampled)"
		}
		fmt.Fprintf(w, "manifest\t%s objects, %s records%s\n",
			orZero(m.TotalObjects), orZero(m.TotalRecords), sampled)
	}
	_ = w.Flush()
}

func printManifest(out io.Writer, resp *backupResponse) {
	if resp.Manifest == nil || len(resp.Manifest.Entries) == 0 {
		fmt.Fprintln(out, "\nno manifest was recorded for this backup")
		return
	}

	entries := make([]manifestEntry, len(resp.Manifest.Entries))
	copy(entries, resp.Manifest.Entries)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Database != entries[j].Database {
			return entries[i].Database < entries[j].Database
		}
		return entries[i].ObjectName < entries[j].ObjectName
	})

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DATABASE\tOBJECT\tRECORDS\tSIZE")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Database, e.ObjectName,
			orZero(e.RecordCount), humanBytes(parseInt64(e.SizeBytes)))
	}
	_ = w.Flush()
}

// parseKeyValues turns repeated key=value flags into a map.
func parseKeyValues(raw []string, what string) (map[string]string, error) {
	parsed := make(map[string]string, len(raw))
	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%s %q is not in key=value form", what, entry)
		}
		parsed[key] = value
	}
	return parsed, nil
}
