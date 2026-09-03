package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newAuditCommand reads the append-only record of who did what.
//
// It exists for the same reason `backup retention` does: the fact is in the database and there is
// no other way to see it. An audit log nobody can read is a table, not a control — and the first
// question after any incident is "who did this, and what else did they try", which is exactly the
// two queries this command makes cheap.
func newAuditCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		actor        string
		action       string
		resourceType string
		resourceID   string
		failuresOnly bool
		limit        int
	)

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Who did what, from the append-only record",
		Long: "Reads `audit_log`, newest first.\n\n" +
			"Every mutating action lands here, whether it succeeded or not, and so does every\n" +
			"refusal of somebody who was authenticated. A request that carried no credential does\n" +
			"not: it names nobody, so the row would say nothing, and it is the one an attacker can\n" +
			"produce a million of. Those are in the control plane's log instead.\n\n" +
			"An actor of `system:scheduler` or `system:retention` is Fleetward's own automatic\n" +
			"work, which has no user behind it and never had one. `bootstrap` is the break-glass\n" +
			"credential from configuration.\n\n" +
			"The table refuses UPDATE and DELETE at the database level. Nothing prunes it either —\n" +
			"see docs/ops/authorization.md for what it grows at.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			query := url.Values{}
			if actor != "" {
				query.Set("actor", actor)
			}
			if action != "" {
				query.Set("action", action)
			}
			if resourceType != "" {
				query.Set("resource_type", resourceType)
			}
			if resourceID != "" {
				query.Set("resource_id", resourceID)
			}
			if failuresOnly {
				query.Set("failures_only", "true")
			}
			if limit > 0 {
				query.Set("page_size", strconv.Itoa(limit))
			}

			var resp struct {
				Entries []struct {
					Actor        string            `json:"actor"`
					Action       string            `json:"action"`
					ResourceType string            `json:"resource_type"`
					ResourceID   string            `json:"resource_id"`
					Details      map[string]string `json:"details"`
					SourceIP     string            `json:"source_ip"`
					Succeeded    bool              `json:"succeeded"`
					OccurredAt   string            `json:"occurred_at"`
				} `json:"entries"`
			}
			c := newClient(*serverURL, *timeout, *token)
			if err := c.get(ctx, "/api/v1/audit", query, &resp); err != nil {
				return err
			}
			if len(resp.Entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching audit records.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WHEN\tACTOR\tACTION\tRESOURCE\tOUTCOME\tWHY")
			for _, e := range resp.Entries {
				outcome := "ok"
				if !e.Succeeded {
					outcome = "REFUSED"
					if e.Details["outcome"] == "failed" {
						outcome = "failed"
					}
				}
				why := e.Details["reason"]
				if why == "" && !e.Succeeded && e.Details["code"] != "" {
					why = e.Details["code"]
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s %s\t%s\t%s\n",
					e.OccurredAt, e.Actor, e.Action, e.ResourceType, short(e.ResourceID), outcome, why)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVar(&actor, "actor", "", "only this actor, e.g. an email or system:retention")
	cmd.Flags().StringVar(&action, "action", "", "only this action, e.g. backup.run")
	cmd.Flags().StringVar(&resourceType, "resource-type", "", "only this kind of resource")
	cmd.Flags().StringVar(&resourceID, "resource-id", "", "only this resource")
	cmd.Flags().BoolVar(&failuresOnly, "failures", false,
		"only refusals and failures, which is where an investigation starts")
	cmd.Flags().IntVar(&limit, "limit", 50, "how many records to show")
	return cmd
}
