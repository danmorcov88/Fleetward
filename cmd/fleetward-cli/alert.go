package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newAlertCommand is the operational surface of alerting.
//
// Until there is a screen, this is the whole of it: what is wrong, what Fleetward is watching for,
// and where it sends the answer. Three groups, and the split is not cosmetic — `alert list` is what
// somebody runs when the pager went off, and `alert rule` and `alert notifier` are what somebody
// runs once and rarely again.
func newAlertCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alert",
		Short: "Alerts, the rules that produce them, and where they are delivered",
		Long: "Fleetward evaluates a handful of conditions over the whole estate on a cadence and\n" +
			"records each one it finds as an alert. A condition that keeps being true updates one\n" +
			"row rather than producing thousands, and an alert resolves itself when the condition\n" +
			"clears.\n\n" +
			"Delivery is best-effort. The alert row is the record; a webhook or an email is a\n" +
			"convenience, and the absence of one is not evidence that nothing is wrong. Run\n" +
			"`alert notifier list` to see whether a destination has been failing.",
	}
	cmd.AddCommand(
		newAlertListCommand(serverURL, timeout, token),
		newAlertAckCommand(serverURL, timeout, token),
		newAlertRuleCommand(serverURL, timeout, token),
		newAlertNotifierCommand(serverURL, timeout, token),
	)
	return cmd
}

// -----------------------------------------------------------------------------------------------
// Alerts
// -----------------------------------------------------------------------------------------------

func newAlertListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		instanceName    string
		includeResolved bool
		minSeverity     string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "What is wrong right now",
		Long: "Open alerts, loudest first. A caller granted a few instances sees those instances'\n" +
			"alerts rather than a refusal.\n\n" +
			"An estate-wide alert — the retention sweep being unable to delete anything, for\n" +
			"instance — belongs to no server, so it is shown only to somebody who can see the\n" +
			"whole tenant.",
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
			if includeResolved {
				query.Set("include_resolved", "true")
			}
			if minSeverity != "" {
				query.Set("min_severity", alertSeverityEnum(minSeverity))
			}

			var resp alertListResponse
			if err := c.get(ctx, "/api/v1/alerts", query, &resp); err != nil {
				return err
			}
			printAlerts(cmd.OutOrStdout(), resp.Alerts, includeResolved)
			return nil
		},
	}

	cmd.Flags().StringVar(&instanceName, "instance", "", "instance name or identifier")
	cmd.Flags().BoolVar(&includeResolved, "include-resolved", false,
		"also show alerts whose condition has cleared")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "",
		"only alerts at this severity or above: info, warning, critical")
	return cmd
}

func newAlertAckCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ack <alert-id>",
		Short: "Say that you know about an alert",
		Long: "Acknowledging silences an alert until its condition clears. It does not resolve it:\n" +
			"only evaluation resolves an alert, because only evaluation knows whether the thing is\n" +
			"still broken.\n\n" +
			"Acknowledging is a mutating action and lands in the audit log under your name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout, *token)
			var resp alertAckResponse
			if err := c.post(ctx, "/api/v1/alerts/"+url.PathEscape(args[0])+"/acknowledge",
				map[string]string{}, &resp); err != nil {
				return err
			}
			if resp.Alert == nil {
				return fmt.Errorf("the control plane acknowledged alert %s and returned nothing", args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "acknowledged %s — %s\n",
				resp.Alert.ID, resp.Alert.Summary)
			if resp.Alert.State == "ALERT_STATE_RESOLVED" {
				fmt.Fprintln(cmd.OutOrStdout(),
					"note: this alert had already resolved; nothing was changed")
			}
			return nil
		},
	}
	return cmd
}

func printAlerts(out io.Writer, alerts []alertRow, includeResolved bool) {
	if len(alerts) == 0 {
		if includeResolved {
			fmt.Fprintln(out, "no alerts")
		} else {
			fmt.Fprintln(out, "nothing is firing")
		}
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEVERITY\tSTATE\tSINCE\tINSTANCE\tSUMMARY\tID")
	for _, a := range alerts {
		instance := a.InstanceName
		if instance == "" {
			// An estate-wide condition belongs to no server, and an empty column reads as missing
			// data rather than as the answer.
			instance = "(the estate)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			strings.ToLower(trimEnum("ALERT_SEVERITY_", a.Severity)),
			strings.ToLower(trimEnum("ALERT_STATE_", a.State)),
			relativeTime(a.StartedAt),
			instance,
			a.Summary,
			a.ID)
	}
	_ = w.Flush()

	// The detail is where the sentence an operator can act on lives, and a table cannot hold it.
	for _, a := range alerts {
		if a.Detail == "" {
			continue
		}
		fmt.Fprintf(out, "\n%s\n  %s\n", a.Summary, a.Detail)
	}
}

// -----------------------------------------------------------------------------------------------
// Rules
// -----------------------------------------------------------------------------------------------

func newAlertRuleCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rule",
		Short: "What Fleetward is watching for",
		Long: "A rule says which condition is worth being told about, at what severity, over which\n" +
			"part of the estate. A fresh installation has three: a failed verification, a missed\n" +
			"backup window, and an instance that stopped answering.\n\n" +
			"Two rules covering the same broken thing produce one alert, at the higher of the two\n" +
			"severities. Nobody is woken twice for one problem.",
	}
	cmd.AddCommand(
		newAlertRuleListCommand(serverURL, timeout, token),
		newAlertRuleCreateCommand(serverURL, timeout, token),
		newAlertRuleEnabledCommand(serverURL, timeout, token, true),
		newAlertRuleEnabledCommand(serverURL, timeout, token, false),
		newAlertRuleDeleteCommand(serverURL, timeout, token),
	)
	return cmd
}

func newAlertRuleListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var includeDisabled bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the alert rules in this tenant",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			query := url.Values{}
			if includeDisabled {
				query.Set("include_disabled", "true")
			}
			var resp alertRuleListResponse
			if err := c(serverURL, timeout, token).get(ctx, "/api/v1/alert-rules", query, &resp); err != nil {
				return err
			}
			printAlertRules(cmd.OutOrStdout(), resp.Rules)
			return nil
		},
	}
	cmd.Flags().BoolVar(&includeDisabled, "include-disabled", false, "also show rules that are switched off")
	return cmd
}

func newAlertRuleCreateCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		name         string
		description  string
		kind         string
		severity     string
		threshold    float64
		instanceName string
		environment  string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Declare a condition worth being told about",
		Long: "Kinds that are evaluated today:\n\n" +
			"  verification_failed   a backup was restored into a sandbox and proved unusable\n" +
			"  backup_missing        a backup was expected inside a declared window and none came\n" +
			"  backup_failed         the most recent backup attempt on an instance failed\n" +
			"  instance_down         an instance stopped answering its health probe\n" +
			"  retention_blocked     expired artifacts the object store will not let us delete\n\n" +
			"`storage_threshold`, `replication_lag` and `custom_promql` exist in the schema and\n" +
			"have no evaluator yet. Creating one is refused rather than accepted, because a rule\n" +
			"that is stored and never fires is worse than one that is refused: you would believe\n" +
			"you were covered.\n\n" +
			"A rule with neither --instance nor --environment covers the whole tenant.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			client := c(serverURL, timeout, token)
			body := map[string]any{
				"name":        name,
				"description": description,
				"kind":        alertKindEnum(kind),
			}
			if severity != "" {
				body["severity"] = alertSeverityEnum(severity)
			}
			if threshold != 0 {
				body["threshold"] = threshold
			}
			if instanceName != "" {
				inst, err := resolveInstance(ctx, client, instanceName)
				if err != nil {
					return err
				}
				body["instance_id"] = inst.ID
			}
			if environment != "" {
				env, err := resolveEnvironment(ctx, client, environment)
				if err != nil {
					return err
				}
				body["environment_id"] = env.ID
			}

			var resp alertRuleResponse
			if err := client.post(ctx, "/api/v1/alert-rules", body, &resp); err != nil {
				return err
			}
			if resp.Rule == nil {
				return fmt.Errorf("the control plane created the rule and returned nothing")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created rule %s (%s, %s)\n",
				resp.Rule.Name,
				strings.ToLower(trimEnum("ALERT_KIND_", resp.Rule.Kind)),
				strings.ToLower(trimEnum("ALERT_SEVERITY_", resp.Rule.Severity)))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "what to call this rule (required)")
	cmd.Flags().StringVar(&description, "description", "", "why it exists")
	cmd.Flags().StringVar(&kind, "kind", "", "the condition to evaluate (required)")
	cmd.Flags().StringVar(&severity, "severity", "", "info, warning or critical")
	cmd.Flags().Float64Var(&threshold, "threshold", 0,
		"the number this kind compares against; retention_blocked reads it as hours")
	cmd.Flags().StringVar(&instanceName, "instance", "", "restrict the rule to one instance")
	cmd.Flags().StringVar(&environment, "environment", "", "restrict the rule to one environment")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

// newAlertRuleEnabledCommand builds both `enable` and `disable`, which differ by one boolean and by
// the sentence they print.
func newAlertRuleEnabledCommand(serverURL *string, timeout *time.Duration, token *string, enable bool) *cobra.Command {
	use, short := "disable <rule-id>", "Stop evaluating a rule, and resolve what it opened"
	if enable {
		use, short = "enable <rule-id>", "Resume evaluating a rule"
	}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: "Disabling a rule is the whole silencing story in this version, and it is honest about\n" +
			"being that: there are no maintenance windows and no per-alert snoozes yet.\n\n" +
			"A disabled rule's open alerts are resolved on the next evaluation pass. \"Stop telling\n" +
			"me\" has to mean the row goes quiet rather than that it freezes where it was.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp alertRuleResponse
			if err := c(serverURL, timeout, token).post(ctx,
				"/api/v1/alert-rules/"+url.PathEscape(args[0])+"/enabled",
				map[string]any{"enabled": enable}, &resp); err != nil {
				return err
			}
			if enable {
				fmt.Fprintf(cmd.OutOrStdout(), "enabled %s\n", ruleName(resp.Rule, args[0]))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"disabled %s; its open alerts resolve on the next evaluation pass\n",
				ruleName(resp.Rule, args[0]))
			return nil
		},
	}
	return cmd
}

func newAlertRuleDeleteCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <rule-id>",
		Short: "Remove a rule and resolve the alerts it opened",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp alertRuleDeleteResponse
			if err := c(serverURL, timeout, token).delete(ctx,
				"/api/v1/alert-rules/"+url.PathEscape(args[0]), nil, &resp); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted rule %s\n", args[0])
			if resp.AlertsResolved > 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"resolved %d alert(s) it had opened; without this they would stay firing with "+
						"nothing evaluating them\n", resp.AlertsResolved)
			}
			return nil
		},
	}
	return cmd
}

func printAlertRules(out io.Writer, rules []alertRuleRow) {
	if len(rules) == 0 {
		fmt.Fprintln(out, "no alert rules; nothing will ever fire")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tSEVERITY\tSCOPE\tENABLED\tID")
	for _, r := range rules {
		scope := "the estate"
		switch {
		case r.InstanceID != "":
			scope = "instance " + r.InstanceID
		case r.EnvironmentID != "":
			scope = "environment " + r.EnvironmentID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\n",
			r.Name,
			strings.ToLower(trimEnum("ALERT_KIND_", r.Kind)),
			strings.ToLower(trimEnum("ALERT_SEVERITY_", r.Severity)),
			scope, r.IsEnabled, r.ID)
	}
	_ = w.Flush()
}

func ruleName(rule *alertRuleRow, fallback string) string {
	if rule != nil && rule.Name != "" {
		return rule.Name
	}
	return fallback
}

// -----------------------------------------------------------------------------------------------
// Notifiers
// -----------------------------------------------------------------------------------------------

func newAlertNotifierCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notifier",
		Short: "Where alerts are delivered",
		Long: "A notifier is a webhook or an SMTP destination. Its one credential goes to the\n" +
			"secrets store and is never returned by any read; everything else about it is\n" +
			"configuration you can see.\n\n" +
			"Delivery is at-most-once and best-effort. `notifier list` reports when each\n" +
			"destination was last tried, last succeeded, and what went wrong — which is how a\n" +
			"webhook that has been failing all week becomes visible rather than silent.",
	}
	cmd.AddCommand(
		newNotifierListCommand(serverURL, timeout, token),
		newNotifierCreateCommand(serverURL, timeout, token),
		newNotifierDeleteCommand(serverURL, timeout, token),
		newNotifierTestCommand(serverURL, timeout, token),
	)
	return cmd
}

func newNotifierListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var includeDisabled bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List delivery destinations and how they have been doing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			query := url.Values{}
			if includeDisabled {
				query.Set("include_disabled", "true")
			}
			var resp notifierListResponse
			if err := c(serverURL, timeout, token).get(ctx, "/api/v1/notifiers", query, &resp); err != nil {
				return err
			}
			printNotifiers(cmd.OutOrStdout(), resp.Notifiers)
			return nil
		},
	}
	cmd.Flags().BoolVar(&includeDisabled, "include-disabled", false, "also show destinations that are switched off")
	return cmd
}

func newNotifierCreateCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		name        string
		kind        string
		settings    []string
		minSeverity string
		secretStdin bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Add a webhook or SMTP destination",
		Long: "Settings for a webhook:\n\n" +
			"  url           where to POST (required)\n" +
			"  auth_header   which header carries the credential; defaults to Authorization\n\n" +
			"Settings for smtp:\n\n" +
			"  host, port    the server; port defaults to 587, or 465 for implicit TLS\n" +
			"  from, to      the sender and a comma-separated recipient list (required)\n" +
			"  username      omit for a relay that needs no authentication\n" +
			"  tls           starttls (the default), implicit, or none\n\n" +
			"The credential is read from FLEETWARD_NOTIFIER_SECRET, or from stdin with\n" +
			"--secret-stdin. It never goes in --setting: settings are returned by `notifier list`,\n" +
			"and a credential-shaped key there is refused rather than quietly stored.\n\n" +
			"One caveat this cannot catch: a Slack or Teams webhook URL embeds its own token, and\n" +
			"that URL lives in settings.url where any administrator can read it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			parsed, err := parseSettings(settings)
			if err != nil {
				return err
			}
			secret, err := readNotifierSecret(cmd, secretStdin)
			if err != nil {
				return err
			}

			body := map[string]any{
				"name":     name,
				"kind":     kind,
				"settings": parsed,
			}
			if secret != "" {
				body["secret"] = secret
			}
			if minSeverity != "" {
				body["min_severity"] = alertSeverityEnum(minSeverity)
			}

			var resp notifierResponse
			if err := c(serverURL, timeout, token).post(ctx, "/api/v1/notifiers", body, &resp); err != nil {
				return err
			}
			if resp.Notifier == nil {
				return fmt.Errorf("the control plane created the notifier and returned nothing")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created notifier %s (%s)\n", resp.Notifier.Name, resp.Notifier.Kind)
			fmt.Fprintf(cmd.OutOrStdout(),
				"run `fleetward-cli alert notifier test %s` before you rely on it\n", resp.Notifier.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "what to call this destination (required)")
	cmd.Flags().StringVar(&kind, "kind", "", "webhook or smtp (required)")
	cmd.Flags().StringArrayVar(&settings, "setting", nil, "key=value, repeatable")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "",
		"do not deliver alerts below this severity: info, warning, critical")
	cmd.Flags().BoolVar(&secretStdin, "secret-stdin", false, "read the credential from stdin")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

func newNotifierDeleteCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <notifier-id>",
		Short: "Remove a destination and the credential it held",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp struct{}
			if err := c(serverURL, timeout, token).delete(ctx,
				"/api/v1/notifiers/"+url.PathEscape(args[0]), nil, &resp); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted notifier %s\n", args[0])
			return nil
		},
	}
}

func newNotifierTestCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "test <notifier-id>",
		Short: "Send a real message to a real endpoint",
		Long: "There are two moments to find out whether a destination is configured correctly: now,\n" +
			"or during the incident it was configured for. This is the first one.\n\n" +
			"A message really is sent. The result is also recorded on the notifier's row, so\n" +
			"`notifier list` shows it afterwards.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp notifierTestResponse
			if err := c(serverURL, timeout, token).post(ctx,
				"/api/v1/notifiers/"+url.PathEscape(args[0])+"/test", map[string]any{}, &resp); err != nil {
				return err
			}
			if resp.Delivered {
				fmt.Fprintf(cmd.OutOrStdout(), "delivered in %s\n", resp.Duration)
				return nil
			}
			// A failed delivery is an answer to the question, not a failure to answer it — but the
			// exit code should still be non-zero, because a script testing a notifier wants to know.
			return fmt.Errorf("not delivered: %s", resp.Error)
		},
	}
}

func printNotifiers(out io.Writer, notifiers []notifierRow) {
	if len(notifiers) == 0 {
		fmt.Fprintln(out, "no notifiers; alerts are recorded and delivered nowhere")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tMIN SEVERITY\tSECRET\tLAST OK\tLAST ERROR\tID")
	for _, n := range notifiers {
		secret := "none"
		if n.HasSecret {
			secret = "stored"
		}
		lastError := n.LastError
		if lastError == "" {
			lastError = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, n.Kind,
			strings.ToLower(trimEnum("ALERT_SEVERITY_", n.MinSeverity)),
			secret, relativeTime(n.LastSuccessAt), lastError, n.ID)
	}
	_ = w.Flush()

	// Said once, under the table, because it is the one thing about delivery an operator has to
	// carry around with them.
	fmt.Fprintln(out,
		"\ndelivery is best-effort: the alert row is the record, and the absence of a notification "+
			"is not evidence that nothing is wrong")
}

// -----------------------------------------------------------------------------------------------
// Plumbing
// -----------------------------------------------------------------------------------------------

// c is a shorthand for the client the commands in this file build, which they do identically.
func c(serverURL *string, timeout *time.Duration, token *string) *client {
	return newClient(*serverURL, *timeout, *token)
}

// parseSettings turns repeated key=value flags into the map the API takes.
func parseSettings(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--setting %q is not key=value", pair)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}

// readNotifierSecret takes the credential from stdin or the environment, never from a flag.
//
// The same rule `instance add` already follows for a database password: a flag is in the shell's
// history, in the process list, and in whatever ships the shell's history somewhere else.
func readNotifierSecret(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read the credential from stdin: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	return os.Getenv("FLEETWARD_NOTIFIER_SECRET"), nil
}

// alertKindEnum and alertSeverityEnum turn what a person types into what the contract spells.
//
// The CLI speaks the vocabulary of the schema — `verification_failed`, `critical` — and the REST
// surface renders enums by name, so the two need one translation and it lives here rather than in
// every command.
func alertKindEnum(kind string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(kind))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "ALERT_KIND_") {
		return trimmed
	}
	return "ALERT_KIND_" + trimmed
}

func alertSeverityEnum(severity string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(severity))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "ALERT_SEVERITY_") {
		return trimmed
	}
	return "ALERT_SEVERITY_" + trimmed
}

// relativeTime renders an instant the way somebody reading a terminal wants it.
func relativeTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "never"
	}
	since := time.Since(*at)
	switch {
	case since < time.Minute:
		return "just now"
	case since < time.Hour:
		return fmt.Sprintf("%dm ago", int(since.Minutes()))
	case since < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(since.Hours()))
	default:
		return at.UTC().Format("2006-01-02 15:04Z")
	}
}
