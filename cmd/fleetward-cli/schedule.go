package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// schedule is one recurring intent, as the API returns it.
type schedule struct {
	ID                  string            `json:"id"`
	InstanceID          string            `json:"instance_id"`
	Kind                string            `json:"kind"`
	CronExpression      string            `json:"cron_expression"`
	Timezone            string            `json:"timezone"`
	MethodID            string            `json:"method_id"`
	Options             map[string]string `json:"options"`
	VerifyPolicy        string            `json:"verify_policy"`
	VerifySamplePercent int32             `json:"verify_sample_percent"`
	RetentionDays       int32             `json:"retention_days"`
	IsEnabled           bool              `json:"is_enabled"`
	NextRunAt           string            `json:"next_run_at"`
	LastRunAt           string            `json:"last_run_at"`
	// The declaration half: when a backup is supposed to have happened, which is a different
	// question from CronExpression, the cadence this schedule itself runs on.
	ExpectedCron         string `json:"expected_cron"`
	ExpectedGraceMinutes int32  `json:"expected_grace_minutes"`
}

// job is one run the scheduler leased.
type job struct {
	ID           string `json:"id"`
	InstanceID   string `json:"instance_id"`
	ScheduleID   string `json:"schedule_id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	LeaseOwner   string `json:"lease_owner"`
	Attempts     int32  `json:"attempts"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	ErrorMessage string `json:"error_message"`
	CreatedAt    string `json:"created_at"`
}

// newScheduleCommand builds the `schedule` group.
func newScheduleCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Declare when backups run without anyone asking",
		Long: "A schedule is recurring intent, not a run. The control plane turns it into a job when\n" +
			"it falls due, leases that job, and records what happened — which is what `job list`\n" +
			"shows.\n\n" +
			"Cron expressions are interpreted in the schedule's timezone, so `0 2 * * *` with\n" +
			"--timezone Europe/Bucharest means 02:00 where the server is, in summer and in winter.",
	}
	cmd.AddCommand(
		newScheduleCreateCommand(serverURL, timeout, token),
		newScheduleListCommand(serverURL, timeout, token),
		newScheduleEnabledCommand(serverURL, timeout, token, true),
		newScheduleEnabledCommand(serverURL, timeout, token, false),
		newScheduleDeleteCommand(serverURL, timeout, token),
	)
	return cmd
}

func newScheduleCreateCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		instanceName  string
		kind          string
		cronExpr      string
		timezone      string
		methodID      string
		options       []string
		verifyPolicy  string
		samplePercent int32
		retentionDays int32
		expectCron    string
		expectGrace   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a backup, observation, or health-probe schedule for one instance",
		Long: "Three kinds of schedule run today.\n\n" +
			"  backup     Fleetward takes the backup, on the cadence --cron names.\n" +
			"  observe    Fleetward reads the engine's own record of backups it did not take, on the\n" +
			"             cadence --cron names, and changes nothing on the instance.\n" +
			"  discovery  Fleetward probes the instance and records its health, so the estate view\n" +
			"             shows an answer somebody can put a date on rather than whatever the last\n" +
			"             person to run `instance test` left behind.\n\n" +
			"--expect-cron is separate from --cron and answers a different question. --cron is how\n" +
			"often Fleetward looks; --expect-cron is when a backup is supposed to have happened, and\n" +
			"it is what `backup adherence` holds the instance to. Without it Fleetward can report\n" +
			"what it found and cannot report what is missing.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsedOptions, err := parseKeyValues(options, "option")
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout, *token)
			inst, err := resolveInstance(ctx, c, instanceName)
			if err != nil {
				return err
			}

			switch strings.ToLower(kind) {
			case "backup", "observe", "discovery":
			default:
				return fmt.Errorf("--kind must be %q, %q or %q, got %q",
					"backup", "observe", "discovery", kind)
			}
			if expectGrace < 0 {
				return fmt.Errorf("--expect-grace cannot be negative")
			}
			if expectCron == "" && expectGrace != 0 {
				return fmt.Errorf("--expect-grace means nothing without --expect-cron: it says how " +
					"late a backup may be, and nothing has said when one is due")
			}

			body := map[string]any{
				"instance_id":     inst.ID,
				"kind":            "JOB_KIND_" + strings.ToUpper(kind),
				"cron_expression": cronExpr,
				"timezone":        timezone,
				"verify_policy":   "VERIFY_POLICY_" + strings.ToUpper(verifyPolicy),
			}
			if expectCron != "" {
				body["expected_cron"] = expectCron
				body["expected_grace_minutes"] = int32(expectGrace.Minutes())
			}
			if methodID != "" {
				body["method_id"] = methodID
			}
			if len(parsedOptions) > 0 {
				body["options"] = parsedOptions
			}
			if samplePercent > 0 {
				body["verify_sample_percent"] = samplePercent
			}
			if retentionDays > 0 {
				body["retention_days"] = retentionDays
			}

			var resp struct {
				Schedule schedule `json:"schedule"`
			}
			if err := c.post(ctx, "/api/v1/instances/"+inst.ID+"/schedules", body, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "schedule %s created on %s\n", resp.Schedule.ID, inst.Name)
			fmt.Fprintf(out, "  %s in %s — next run %s\n",
				resp.Schedule.CronExpression, resp.Schedule.Timezone, formatRunTime(resp.Schedule.NextRunAt))
			// The expectation is about backups, so the hint about a missing one belongs only to a
			// schedule that has anything to do with backups. A health probe told to declare when a
			// backup is due sends the reader looking for a flag that would do nothing.
			switch {
			case resp.Schedule.ExpectedCron != "":
				fmt.Fprintf(out, "  a backup is expected at %s in %s, up to %s late\n",
					resp.Schedule.ExpectedCron, resp.Schedule.Timezone,
					formatGrace(resp.Schedule.ExpectedGraceMinutes))
			case strings.EqualFold(kind, "discovery"):
				fmt.Fprintln(out, "  this instance's health will be probed on that cadence, and the "+
					"estate view shows how old the answer is")
			default:
				fmt.Fprintln(out, "  nothing was declared about when a backup is due, so "+
					"`backup adherence` cannot report this instance as behind — add --expect-cron")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier (required)")
	cmd.Flags().StringVar(&kind, "kind", "backup",
		"backup, observe to read backups Fleetward did not take, or discovery to probe health")
	cmd.Flags().StringVar(&cronExpr, "cron", "", "standard five-field cron expression, e.g. \"0 2 * * *\" (required)")
	cmd.Flags().StringVar(&expectCron, "expect-cron", "",
		"when a backup is supposed to have happened, as a cron expression; what `backup adherence` checks")
	cmd.Flags().DurationVar(&expectGrace, "expect-grace", 0,
		"how late a backup may be and still count, e.g. 2h; defaults to 2h when --expect-cron is set")
	cmd.Flags().StringVar(&timezone, "timezone", "UTC", "IANA timezone the expression is read in, e.g. Europe/Bucharest")
	cmd.Flags().StringVar(&methodID, "method", "", "backup method; empty uses the plugin's default")
	cmd.Flags().StringArrayVar(&options, "option", nil, "method option as key=value; repeatable")
	cmd.Flags().StringVar(&verifyPolicy, "verify", "always",
		"when to prove the backup restorable: always, sampled, or manual")
	cmd.Flags().Int32Var(&samplePercent, "verify-percent", 0, "percentage to verify when --verify=sampled")
	cmd.Flags().Int32Var(&retentionDays, "retention-days", 0, "how long artifacts are kept; 0 uses the default")
	_ = cmd.MarkFlagRequired("instance")
	_ = cmd.MarkFlagRequired("cron")

	return cmd
}

func newScheduleListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var instanceName string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List schedules",
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

			var resp struct {
				Schedules []schedule `json:"schedules"`
			}
			if err := c.get(ctx, "/api/v1/schedules", query, &resp); err != nil {
				return err
			}
			if len(resp.Schedules) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No schedules yet. Nothing runs automatically until one exists.\n"+
						"Create one with `fleetward-cli schedule create --instance <name> --cron \"0 2 * * *\"`.")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tCRON\tEXPECTED\tTIMEZONE\tVERIFY\tENABLED\tNEXT RUN (UTC)\tLAST RUN (UTC)")
			for _, s := range resp.Schedules {
				expected := s.ExpectedCron
				if expected == "" {
					expected = "—"
				} else {
					expected += " ±" + formatGrace(s.ExpectedGraceMinutes)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID,
					strings.ToLower(trimEnum("JOB_KIND_", s.Kind)),
					s.CronExpression,
					expected,
					s.Timezone,
					verifyLabel(s),
					yesNo(s.IsEnabled),
					formatRunTime(s.NextRunAt),
					formatRunTime(s.LastRunAt))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "restrict to one instance, by name or identifier")
	return cmd
}

// newScheduleEnabledCommand builds `schedule enable` and `schedule disable`, which are the same
// call with a different argument. Pausing a schedule during a migration window is routine, and
// deleting and recreating one to achieve it would lose the history pointing at it.
func newScheduleEnabledCommand(serverURL *string, timeout *time.Duration, token *string, enable bool) *cobra.Command {
	verb, past := "disable", "disabled"
	if enable {
		verb, past = "enable", "enabled"
	}

	return &cobra.Command{
		Use:   verb + " <schedule-id>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a schedule without deleting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp struct {
				Schedule schedule `json:"schedule"`
			}
			if err := newClient(*serverURL, *timeout, *token).post(ctx,
				"/api/v1/schedules/"+args[0]+"/enabled",
				map[string]any{"schedule_id": args[0], "enabled": enable}, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "schedule %s %s\n", args[0], past)
			if enable {
				// Resuming recomputes the next run from now, so an operator sees when it actually
				// resumes rather than assuming it caught up on what it missed.
				fmt.Fprintf(out, "  next run %s\n", formatRunTime(resp.Schedule.NextRunAt))
			}
			return nil
		},
	}
}

func newScheduleDeleteCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <schedule-id>",
		Short: "Delete a schedule, keeping the jobs it already ran",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			if err := newClient(*serverURL, *timeout, *token).
				delete(ctx, "/api/v1/schedules/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "schedule %s deleted\n", args[0])
			return nil
		},
	}
}

// newJobCommand builds the `job` group.
//
// This is the only surface on which the scheduler is visible at all: a run that was skipped because
// the previous one had not finished, a job another replica leased, or a job the reaper closed after
// a crash all appear here and nowhere else.
func newJobCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Inspect what the scheduler did",
	}
	cmd.AddCommand(newJobListCommand(serverURL, timeout, token))
	return cmd
}

func newJobListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		instanceName string
		state        string
		limit        int32
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs, most recent first",
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
			if state != "" {
				query.Set("state", "JOB_STATE_"+strings.ToUpper(state))
			}
			if limit > 0 {
				query.Set("page_size", fmt.Sprint(limit))
			}

			var resp struct {
				Jobs []job `json:"jobs"`
			}
			if err := c.get(ctx, "/api/v1/jobs", query, &resp); err != nil {
				return err
			}
			if len(resp.Jobs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No jobs yet.")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tSTATE\tTRIGGER\tATTEMPTS\tSTARTED (UTC)\tFINISHED (UTC)\tERROR")
			for _, j := range resp.Jobs {
				trigger := "manual"
				if j.ScheduleID != "" {
					trigger = "schedule"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
					j.ID,
					strings.ToLower(trimEnum("JOB_KIND_", j.Kind)),
					strings.ToLower(trimEnum("JOB_STATE_", j.State)),
					trigger,
					j.Attempts,
					formatRunTime(j.StartedAt),
					formatRunTime(j.FinishedAt),
					firstLine(j.ErrorMessage))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "restrict to one instance, by name or identifier")
	cmd.Flags().StringVar(&state, "state", "", "restrict to pending, running, succeeded, failed, or canceled")
	cmd.Flags().Int32Var(&limit, "limit", 0, "how many jobs to show; 0 uses the server's default")
	return cmd
}

// verifyLabel renders the verification policy the way a DBA would say it out loud.
func verifyLabel(s schedule) string {
	policy := strings.ToLower(trimEnum("VERIFY_POLICY_", s.VerifyPolicy))
	if policy == "sampled" {
		return fmt.Sprintf("sampled %d%%", s.VerifySamplePercent)
	}
	return policy
}

// formatRunTime renders an RFC 3339 timestamp in UTC, or a dash when there is none.
//
// UTC without exception, including for a schedule written in another timezone: the local time is
// what the cron expression says, and showing a second local time next to it is how someone ends up
// reading one as the other.
func formatRunTime(value string) string {
	if value == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// firstLine trims a multi-line error down to something a table can hold.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	const max = 60
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
