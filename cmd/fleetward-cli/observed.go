package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newBackupHistoryCommand lists backups of both origins.
func newBackupHistoryCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var (
		instanceName string
		origin       string
		limit        int32
	)

	cmd := &cobra.Command{
		Use:   "history",
		Short: "List backups, including ones Fleetward did not take",
		Long: "Backup history covers both origins.\n\n" +
			"A `managed` backup is one Fleetward ran: it carries a manifest captured at backup time,\n" +
			"a checksum, and an artifact Fleetward controls, and it can be verified. An `observed`\n" +
			"backup is evidence of a backup somebody else took — read from the record the engine\n" +
			"keeps, or from the directory the backups are written to. Fleetward reports it and owns\n" +
			"nothing about it, and it can never be verified.\n\n" +
			"The two are never rendered as the same thing, because they are not.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			query := url.Values{}

			if instanceName != "" {
				inst, err := resolveInstance(ctx, c, instanceName)
				if err != nil {
					return err
				}
				query.Set("instance_id", inst.ID)
			}
			switch strings.ToLower(origin) {
			case "":
			case "managed":
				query.Set("origin", "BACKUP_ORIGIN_MANAGED")
			case "observed":
				query.Set("origin", "BACKUP_ORIGIN_OBSERVED")
			default:
				return fmt.Errorf("--origin must be %q or %q, got %q", "managed", "observed", origin)
			}
			if limit > 0 {
				query.Set("page_size", strconv.Itoa(int(limit)))
			}

			var resp backupListResponse
			if err := c.get(ctx, "/api/v1/backups", query, &resp); err != nil {
				return err
			}
			printBackupHistory(cmd.OutOrStdout(), resp.Backups)
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier")
	cmd.Flags().StringVar(&origin, "origin", "", "restrict to managed or observed backups")
	cmd.Flags().Int32Var(&limit, "limit", 0, "maximum rows to return")
	return cmd
}

// newBackupObserveCommand reads one instance's backup history now.
func newBackupObserveCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "observe <instance>",
		Short: "Read an instance's own record of backups Fleetward did not take",
		Long: "Reads whatever evidence the engine keeps about backups taken on it, and records what\n" +
			"it finds. Nothing on the instance changes: no artifact is fetched, moved, or deleted,\n" +
			"and nothing is written to the server.\n\n" +
			"This is the on-demand form of an `observe` schedule, which does the same thing on a\n" +
			"cadence. Running it twice does not record anything twice.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			inst, err := resolveInstance(ctx, c, args[0])
			if err != nil {
				return err
			}

			var resp observeResponse
			if err := c.post(ctx, "/api/v1/instances/"+inst.ID+"/observe",
				map[string]any{"instance_id": inst.ID}, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%d new, %d already known\n", resp.Discovered, resp.Updated)
			if resp.Watermark != nil {
				fmt.Fprintf(out, "newest evidence finished at %s\n", resp.Watermark.UTC().Format(time.RFC3339))
			}
			if resp.Discovered == 0 && resp.Updated == 0 {
				fmt.Fprintln(out, "no backups found — check that the instance's backups are where "+
					"Fleetward is looking, and that they are recent enough to be inside the window it reads")
			}
			return nil
		},
	}
	return cmd
}

// newBackupAdherenceCommand answers the question the product exists for.
func newBackupAdherenceCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var (
		instanceName string
		problemsOnly bool
	)

	cmd := &cobra.Command{
		Use:   "adherence",
		Short: "Did every server's backup run when it was supposed to",
		Long: "Compares what was declared against what actually happened, per instance.\n\n" +
			"It counts backups of both origins, because the question does not care who took them.\n" +
			"An instance with nothing declared is reported as such rather than as healthy: on an\n" +
			"estate of fifty servers, \"nobody has said what this one's backups should look like\"\n" +
			"is a finding.\n\n" +
			"Declare an expectation with: fleetward-cli schedule create --kind observe \\\n" +
			"  --instance <name> --cron '*/30 * * * *' --expect-cron '0 2 * * *' --expect-grace 2h",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			query := url.Values{}
			if instanceName != "" {
				inst, err := resolveInstance(ctx, c, instanceName)
				if err != nil {
					return err
				}
				query.Set("instance_id", inst.ID)
			}
			if problemsOnly {
				query.Set("problems_only", "true")
			}

			var resp adherenceResponse
			if err := c.get(ctx, "/api/v1/backup-adherence", query, &resp); err != nil {
				return err
			}
			printAdherence(cmd.OutOrStdout(), resp.Instances)
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier")
	cmd.Flags().BoolVar(&problemsOnly, "problems", false, "show only instances that are not adherent")
	return cmd
}

func printBackupHistory(out io.Writer, backups []backupRow) {
	if len(backups) == 0 {
		fmt.Fprintln(out, "no backups recorded")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tORIGIN\tSTATE\tMETHOD\tFINISHED (UTC)\tSIZE\tVERIFIED")
	for i := range backups {
		b := &backups[i]
		finished := "—"
		if b.CompletedAt != nil {
			finished = b.CompletedAt.UTC().Format("2006-01-02 15:04:05")
			if b.Evidence != nil && b.Evidence.CompletedAtIsApproximate {
				finished += " ~"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID,
			strings.ToLower(trimEnum("BACKUP_ORIGIN_", b.Origin)),
			strings.ToLower(trimEnum("BACKUP_STATE_", b.State)),
			b.MethodID, finished, humanBytes(parseInt64(b.SizeBytes)),
			verifiedColumn(b))
	}
	_ = w.Flush()

	if approximate(backups) {
		fmt.Fprintln(out, "\n~ the engine records this time without an offset, so it may be out by "+
			"up to an hour across a daylight-saving change")
	}
}

// verifiedColumn is the second half of the two-part status, and it never says "no" for an observed
// backup. "Not verified" and "cannot be verified" are different facts, and rendering the second as
// the first would make a DBA go looking for a verification that is never coming (ADR-0015).
func verifiedColumn(b *backupRow) string {
	if trimEnum("BACKUP_ORIGIN_", b.Origin) == "OBSERVED" {
		return "n/a — not ours"
	}
	if b.Verification == nil {
		return "never"
	}
	return strings.ToLower(trimEnum("VERIFICATION_STATUS_", b.Verification.Status))
}

func approximate(backups []backupRow) bool {
	for i := range backups {
		if backups[i].Evidence != nil && backups[i].Evidence.CompletedAtIsApproximate {
			return true
		}
	}
	return false
}

func printAdherence(out io.Writer, instances []instanceAdherence) {
	if len(instances) == 0 {
		fmt.Fprintln(out, "no instances to report on")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE\tENGINE\tEXPECTED\tGRACE\tLAST BACKUP (UTC)\tADHERENCE")
	for i := range instances {
		a := &instances[i]
		expected := a.ExpectedCron
		grace := formatGrace(a.ExpectedGraceMinutes)
		if expected == "" {
			expected, grace = "—", "—"
		} else if a.Timezone != "" && a.Timezone != "UTC" {
			expected += " " + a.Timezone
		}

		last := "—"
		if a.LatestBackup != nil && a.LatestBackup.CompletedAt != nil {
			last = a.LatestBackup.CompletedAt.UTC().Format("2006-01-02 15:04:05")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.InstanceName, a.EngineType,
			expected, grace, last, strings.ToLower(trimEnum("ADHERENCE_STATE_", a.State)))
	}
	_ = w.Flush()

	// Printed under the table rather than squeezed into it. A caveat is a sentence, and truncating
	// it into a column is how it stops being read.
	for i := range instances {
		a := &instances[i]
		for _, caveat := range a.Caveats {
			fmt.Fprintf(out, "\n%s: %s", a.InstanceName, caveat)
		}
	}
	if hasCaveats(instances) {
		fmt.Fprintln(out)
	}
}

func hasCaveats(instances []instanceAdherence) bool {
	for i := range instances {
		if len(instances[i].Caveats) > 0 {
			return true
		}
	}
	return false
}

// formatGrace renders a grace period the way it was most likely typed: "2h" rather than "120m".
func formatGrace(minutes int32) string {
	if minutes <= 0 {
		return "—"
	}
	if minutes%60 == 0 {
		return strconv.Itoa(int(minutes)/60) + "h"
	}
	return strconv.Itoa(int(minutes)) + "m"
}
