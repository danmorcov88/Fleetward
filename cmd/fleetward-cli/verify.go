package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// verifyPollInterval is how often `backup verify` asks whether the verification has finished. It is
// slower than the backup poll because a verification's first minute is spent pulling an image and
// waiting for a database to initialize, during which there is nothing new to report.
const verifyPollInterval = 3 * time.Second

// newBackupVerifyCommand builds `backup verify`.
//
// It is a subcommand of `backup` rather than a group of its own, because a verification is not a
// thing an operator has independently of the backup it proves. The question being asked is always
// "is this backup any good".
func newBackupVerifyCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var (
		backupID    string
		checks      []string
		wait        bool
		waitTimeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Restore a backup into a throwaway sandbox and check it against its manifest",
		Long: "Fleetward provisions an isolated container of the engine version that produced the\n" +
			"artifact, restores the backup into it, compares what arrived against the manifest\n" +
			"captured when the backup was taken, and destroys the container.\n\n" +
			"Three outcomes are possible, and they mean different things:\n" +
			"  VERIFIED      the restored copy matches the manifest\n" +
			"  FAILED        it does not — this backup is not what it claims to be\n" +
			"  INCONCLUSIVE  the question could not be answered, e.g. the sandbox never started",
		RunE: func(cmd *cobra.Command, _ []string) error {
			startCtx, cancelStart := context.WithTimeout(cmd.Context(), *timeout)
			defer cancelStart()

			c := newClient(*serverURL, *timeout)

			body := map[string]any{}
			if len(checks) > 0 {
				named, err := verificationChecks(checks)
				if err != nil {
					return err
				}
				body["checks"] = named
			}

			var started struct {
				VerificationID string `json:"verification_id"`
				JobID          string `json:"job_id"`
			}
			if err := c.post(startCtx, "/api/v1/backups/"+backupID+"/verify", body, &started); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "verification %s started for backup %s\n", started.VerificationID, backupID)
			if !wait {
				return nil
			}

			// Its own deadline, not the per-request one: a verification pulls an image and starts a
			// database, which takes far longer than the control plane is allowed to take answering
			// a question about it.
			waitCtx, cancelWait := context.WithTimeout(cmd.Context(), waitTimeout)
			defer cancelWait()

			result, err := followVerification(waitCtx, c, started.VerificationID)
			if err != nil {
				return err
			}

			printVerification(out, result)

			// The exit status matters more here than anywhere else in this CLI: a verification is
			// exactly the thing someone will run from a script and act on.
			switch trimEnum("VERIFICATION_STATUS_", result.Status) {
			case "VERIFIED":
				return nil
			case "FAILED":
				return fmt.Errorf("backup %s FAILED verification", backupID)
			default:
				return fmt.Errorf("verification of backup %s was inconclusive", backupID)
			}
		},
	}

	cmd.Flags().StringVar(&backupID, "backup", "", "backup identifier (required)")
	cmd.Flags().StringSliceVar(&checks, "check", nil,
		"check to run: connectivity, schema-presence, record-counts; repeatable, empty runs every check the plugin supports")
	cmd.Flags().BoolVar(&wait, "wait", true, "follow the verification until it finishes")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", time.Hour, "how long to follow a running verification")
	_ = cmd.MarkFlagRequired("backup")

	return cmd
}

// verificationChecks turns the flag's readable names into the contract's enum values.
//
// The CLI accepts the short form because "record-counts" is what an operator would type, and the
// enum name is an artifact of the wire format rather than something anybody should have to know.
func verificationChecks(requested []string) ([]string, error) {
	names := map[string]string{
		"connectivity":    "VERIFICATION_CHECK_CONNECTIVITY",
		"record-counts":   "VERIFICATION_CHECK_RECORD_COUNTS",
		"schema-presence": "VERIFICATION_CHECK_SCHEMA_PRESENCE",
		"integrity":       "VERIFICATION_CHECK_INTEGRITY",
		"queryability":    "VERIFICATION_CHECK_QUERYABILITY",
	}

	out := make([]string, 0, len(requested))
	for _, raw := range requested {
		name, ok := names[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return nil, fmt.Errorf("unknown check %q; known checks are connectivity, "+
				"schema-presence, record-counts, integrity, queryability", raw)
		}
		out = append(out, name)
	}
	return out, nil
}

// followVerification polls until the verification reaches a conclusion.
func followVerification(ctx context.Context, c *client, verificationID string) (*verification, error) {
	ticker := time.NewTicker(verifyPollInterval)
	defer ticker.Stop()

	for {
		var resp verificationResponse
		if err := c.get(ctx, "/api/v1/verifications/"+verificationID, nil, &resp); err != nil {
			return nil, err
		}
		// UNSPECIFIED is what the row reads as while it is still 'running': the status field
		// describes a conclusion, and there is not one yet.
		if trimEnum("VERIFICATION_STATUS_", resp.Verification.Status) != "UNSPECIFIED" {
			return &resp.Verification, nil
		}

		select {
		case <-ctx.Done():
			// The verification itself is unaffected: it runs in the control plane, not here.
			return nil, fmt.Errorf("stopped waiting for verification %s; it is still running", verificationID)
		case <-ticker.C:
		}
	}
}

func printVerification(out io.Writer, v *verification) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	fmt.Fprintf(w, "id\t%s\n", v.ID)
	fmt.Fprintf(w, "backup\t%s\n", v.BackupID)
	fmt.Fprintf(w, "status\t%s\n", trimEnum("VERIFICATION_STATUS_", v.Status))
	if v.Duration != "" {
		fmt.Fprintf(w, "duration\t%s\n", v.Duration)
	}
	if v.ErrorMessage != "" {
		fmt.Fprintf(w, "error\t%s\n", v.ErrorMessage)
	}
	_ = w.Flush()

	if len(v.Checks) == 0 {
		if v.Report != "" {
			fmt.Fprintf(out, "\n%s\n", v.Report)
		}
		return
	}

	fmt.Fprintln(out)
	cw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(cw, "CHECK\tRESULT\tDETAIL")
	for _, check := range v.Checks {
		result := "pass"
		if !check.Passed {
			result = "FAIL"
		}
		fmt.Fprintf(cw, "%s\t%s\t%s\n",
			strings.ToLower(trimEnum("VERIFICATION_CHECK_", check.Check)), result, check.Message)
	}
	_ = cw.Flush()

	// Discrepancies are the point of the whole exercise: "which table is short, and by how much" is
	// the first thing a DBA needs, and a summary line cannot carry it.
	for _, check := range v.Checks {
		if len(check.Discrepancies) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s discrepancies:\n", strings.ToLower(trimEnum("VERIFICATION_CHECK_", check.Check)))
		dw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(dw, "OBJECT\tEXPECTED\tFOUND\tDETAIL")
		for _, d := range check.Discrepancies {
			fmt.Fprintf(dw, "%s\t%s\t%s\t%s\n", d.ObjectName, orZero(d.Expected), orZero(d.Actual), d.Detail)
		}
		_ = dw.Flush()
	}
}
