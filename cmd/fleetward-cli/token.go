package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The credential commands.
//
// There is no `token generate` that mints one locally, and its absence is deliberate. A credential
// that were valid without the control plane having recorded it would be a credential nobody could
// revoke and nobody could attribute — the exact property that makes the bootstrap token something
// an operator is warned about on every start rather than something to hand out. Every token here
// comes from the server, has a row, and can be revoked.

func newTokenCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Issue, list, and revoke API credentials",
		Long: "A token authenticates a person or a script to the control plane, and carries the\n" +
			"role that decides what they may do.\n\n" +
			"Until sign-in through an identity provider arrives, this is how everybody except the\n" +
			"first administrator gets access. The first administrator uses the bootstrap\n" +
			"credential, which is configuration rather than a stored token — see\n" +
			"docs/ops/authorization.md.",
	}
	cmd.AddCommand(
		newTokenCreateCommand(serverURL, timeout, token),
		newTokenListCommand(serverURL, timeout, token),
		newTokenRevokeCommand(serverURL, timeout, token),
		newWhoamiCommand(serverURL, timeout, token),
	)
	return cmd
}

type apiTokenView struct {
	ID          string      `json:"id"`
	UserID      string      `json:"user_id"`
	Description string      `json:"description"`
	CreatedAt   string      `json:"created_at"`
	ExpiresAt   string      `json:"expires_at"`
	LastUsedAt  string      `json:"last_used_at"`
	RevokedAt   string      `json:"revoked_at"`
	DisplayName string      `json:"display_name"`
	Email       string      `json:"email"`
	Grants      []grantView `json:"grants"`
}

type grantView struct {
	Role          string `json:"role"`
	Rank          int    `json:"rank"`
	EnvironmentID string `json:"environment_id"`
	InstanceID    string `json:"instance_id"`
}

func (g grantView) scope() string {
	switch {
	case g.InstanceID != "":
		return "instance " + short(g.InstanceID)
	case g.EnvironmentID != "":
		return "environment " + short(g.EnvironmentID)
	default:
		return "the whole tenant"
	}
}

func newTokenCreateCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var (
		email       string
		displayName string
		role        string
		environment string
		instance    string
		description string
		ttl         time.Duration
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Issue a token for somebody, creating them if they are new",
		Long: "Issues a credential and grants a role with it.\n\n" +
			"The four roles are seeded in the database and ordered:\n" +
			"  viewer     read-only: inventory, health, backups\n" +
			"  operator   may test connections and trigger discovery; cannot back up or restore\n" +
			"  dba        may run backups, verifications and restores within the granted scope\n" +
			"  admin      full control, including issuing tokens and reading the audit log\n\n" +
			"A grant covers one instance, or one environment, or the whole tenant — never two of\n" +
			"those. Grants add up: a dba grant on one instance raises what its holder may do there\n" +
			"and never lowers what an environment-wide grant already allowed.\n\n" +
			"The secret is printed once and is not stored anywhere it can be read back. It goes to\n" +
			"stdout on its own so it can be redirected into a file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			body := map[string]any{
				"email":        email,
				"display_name": displayName,
				"role":         role,
			}
			if environment != "" {
				body["environment_id"] = environment
			}
			if instance != "" {
				body["instance_id"] = instance
			}
			if description != "" {
				body["description"] = description
			}
			if ttl > 0 {
				body["ttl"] = fmt.Sprintf("%ds", int(ttl.Seconds()))
			}

			var resp struct {
				Token  apiTokenView `json:"token"`
				Secret string       `json:"secret"`
			}
			c := newClient(*serverURL, *timeout, *token)
			if err := c.post(ctx, "/api/v1/tokens", body, &resp); err != nil {
				return err
			}

			// The secret alone on stdout, the explanation on stderr — the same split `keygen` uses,
			// so `> token.txt` captures a usable file and the warning still reaches a human.
			fmt.Fprintln(cmd.OutOrStdout(), resp.Secret)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nIssued to %s (%s), %s within %s.\n"+
					"This secret is shown once and is stored only as a hash. Put it in a file and point\n"+
					"FLEETWARD_TOKEN_FILE at it.\n"+
					"Revoke it with: fleetward token revoke %s\n",
				resp.Token.Email, resp.Token.DisplayName, role, scopeOf(resp.Token.Grants), resp.Token.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "who the token is for; the user is created if new (required)")
	cmd.Flags().StringVar(&displayName, "name", "", "display name, defaults to the email")
	cmd.Flags().StringVar(&role, "role", "", "viewer, operator, dba or admin (required)")
	cmd.Flags().StringVar(&environment, "environment-id", "", "grant only within this environment")
	cmd.Flags().StringVar(&instance, "instance-id", "", "grant only on this instance")
	cmd.Flags().StringVar(&description, "description", "", "what this credential is for")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the token lives; zero means until revoked")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func scopeOf(grants []grantView) string {
	if len(grants) == 0 {
		return "no scope"
	}
	return grants[len(grants)-1].scope()
}

func newTokenListCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	var includeInactive bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Credentials issued in this tenant, and what each may do",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			path := "/api/v1/tokens"
			if includeInactive {
				path += "?include_inactive=true"
			}

			var resp struct {
				Tokens []apiTokenView `json:"tokens"`
			}
			c := newClient(*serverURL, *timeout, *token)
			if err := c.get(ctx, path, nil, &resp); err != nil {
				return err
			}
			if len(resp.Tokens) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No tokens.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tWHO\tROLE\tSCOPE\tLAST USED\tSTATE")
			for _, t := range resp.Tokens {
				role, scope := "—", "—"
				if len(t.Grants) > 0 {
					role = t.Grants[len(t.Grants)-1].Role
					scope = t.Grants[len(t.Grants)-1].scope()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					short(t.ID), t.Email, role, scope, orNever(t.LastUsedAt), tokenState(t))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&includeInactive, "all", false, "include revoked and expired tokens")
	return cmd
}

func tokenState(t apiTokenView) string {
	switch {
	case t.RevokedAt != "":
		return "revoked"
	case t.ExpiresAt != "":
		return "expires " + t.ExpiresAt
	default:
		return "active"
	}
}

func newTokenRevokeCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Stop a credential working",
		Long: "The row is kept rather than deleted, so the audit log's references still resolve and\n" +
			"so the revocation is itself a fact somebody can point at.\n\n" +
			"A revoked credential stops working immediately on the control plane that revoked it,\n" +
			"and within the principal cache TTL on any other replica.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout, *token)
			if err := c.delete(ctx, "/api/v1/tokens/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s.\n", args[0])
			return nil
		},
	}
}

// newWhoamiCommand answers the first question anybody asks when a 403 arrives.
func newWhoamiCommand(serverURL *string, timeout *time.Duration, token *string) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Who this credential is, and what it may do",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			var resp struct {
				Caller struct {
					Kind        string `json:"kind"`
					Actor       string `json:"actor"`
					DisplayName string `json:"display_name"`
					Email       string `json:"email"`
				} `json:"caller"`
				Grants      []grantView `json:"grants"`
				HighestRole string      `json:"highest_role"`
			}
			c := newClient(*serverURL, *timeout, *token)
			if err := c.get(ctx, "/api/v1/me", nil, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n", resp.Caller.Actor)
			if resp.Caller.DisplayName != "" && resp.Caller.DisplayName != resp.Caller.Actor {
				fmt.Fprintf(out, "%s\n", resp.Caller.DisplayName)
			}
			if resp.Caller.Kind == "CALLER_KIND_BOOTSTRAP" {
				fmt.Fprintf(out, "\nThis is the bootstrap credential: tenant-wide administrator, and\n"+
					"configuration rather than a stored token. Issue a real one and remove it.\n")
			}
			fmt.Fprintln(out)
			if len(resp.Grants) == 0 {
				fmt.Fprintln(out, "No grants. This credential can authenticate and do nothing else.")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ROLE\tWITHIN")
			for _, g := range resp.Grants {
				fmt.Fprintf(w, "%s\t%s\n", g.Role, g.scope())
			}
			return w.Flush()
		},
	}
}

// orNever is orDash's answer for a timestamp that has not happened, which reads better than a
// dash on the one column an operator scans to find a credential nobody has ever used.
func orNever(v string) string {
	if v == "" {
		return "never"
	}
	return v
}

// short renders a UUID at the width a human reads it at.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
