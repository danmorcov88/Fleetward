package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newEnvironmentCommand builds the `environment` group.
//
// Environments exist before instances do, and deliberately so: an instance's environment is what
// decides whether a destructive operation needs production confirmation, so it is asked for rather
// than guessed.
func newEnvironmentCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "environment",
		Aliases: []string{"env"},
		Short:   "Manage environments",
		Long: "Environments group instances and mark which of them are production.\n" +
			"Every instance belongs to exactly one.",
	}
	cmd.AddCommand(
		newEnvironmentListCommand(serverURL, timeout, token),
		newEnvironmentAddCommand(serverURL, timeout, token),
	)
	return cmd
}

func newEnvironmentListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			envs, err := listEnvironments(ctx, newClient(*serverURL, *timeout, *token))
			if err != nil {
				return err
			}
			if len(envs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No environments yet. Create one with `fleetward-cli environment add production --production`.")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPRODUCTION\tID\tDESCRIPTION")
			for _, env := range envs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					env.Name, yesNo(env.IsProduction), env.ID, env.Description)
			}
			return tw.Flush()
		},
	}
}

func newEnvironmentAddCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		description  string
		isProduction bool
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp struct {
				Environment environment `json:"environment"`
			}
			body := map[string]any{
				"name":          args[0],
				"description":   description,
				"is_production": isProduction,
			}
			if err := newClient(*serverURL, *timeout, *token).post(ctx, "/api/v1/environments", body, &resp); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "environment %q created (%s)\n",
				resp.Environment.Name, resp.Environment.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "human-readable description")
	cmd.Flags().BoolVar(&isProduction, "production", false,
		"mark as production, which requires stronger confirmation for destructive operations")
	return cmd
}

// listEnvironments walks every page so callers can resolve a name without paginating themselves.
func listEnvironments(ctx context.Context, c *client) ([]environment, error) {
	var all []environment
	pageToken := ""
	for {
		var resp struct {
			Environments  []environment `json:"environments"`
			NextPageToken string        `json:"next_page_token"`
		}
		query := pageQuery(pageToken)
		if err := c.get(ctx, "/api/v1/environments", query, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Environments...)
		if resp.NextPageToken == "" {
			return all, nil
		}
		pageToken = resp.NextPageToken
	}
}

// resolveEnvironment accepts either an identifier or a name, so operators can use whichever they
// have to hand.
func resolveEnvironment(ctx context.Context, c *client, nameOrID string) (*environment, error) {
	envs, err := listEnvironments(ctx, c)
	if err != nil {
		return nil, err
	}

	var matches []environment
	for _, env := range envs {
		if env.ID == nameOrID || strings.EqualFold(env.Name, nameOrID) {
			matches = append(matches, env)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no environment named %q; `fleetward-cli environment list` shows what exists", nameOrID)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%q matches %d environments; use the identifier instead", nameOrID, len(matches))
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
