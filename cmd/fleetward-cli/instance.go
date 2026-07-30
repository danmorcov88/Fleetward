package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// passwordEnvVar is where `instance add` looks for the database password when it is not piped in.
//
// There is deliberately no --password flag. A password in argv is visible to every process on the
// machine through ps and lands in the shell history of whoever typed it, and this command exists
// precisely to handle credentials for production databases.
const passwordEnvVar = "FLEETWARD_DB_PASSWORD" //nolint:gosec // G101: the name of a variable, not a credential

// newInstanceCommand builds the `instance` group.
func newInstanceCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "instance",
		Aliases: []string{"inst"},
		Short:   "Manage database instances",
		Long: "Instances are the database servers Fleetward watches. Adding one stores its\n" +
			"connection details and, separately and encrypted, its credentials.",
	}
	cmd.AddCommand(
		newInstanceAddCommand(serverURL, timeout),
		newInstanceListCommand(serverURL, timeout),
		newInstanceGetCommand(serverURL, timeout),
		newInstanceHealthCommand(serverURL, timeout),
		newInstanceDiscoverCommand(serverURL, timeout),
		newInstanceRemoveCommand(serverURL, timeout),
	)
	return cmd
}

func newInstanceAddCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var (
		environmentName string
		engineType      string
		host            string
		port            int32
		username        string
		database        string
		passwordStdin   bool
		tlsEnabled      bool
		tlsSkipVerify   bool
		labels          []string
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a database instance",
		Long: "Stores an instance and its credentials.\n\n" +
			"The password is read from the " + passwordEnvVar + " environment variable, or from\n" +
			"standard input with --password-stdin. There is no --password flag: a password in a\n" +
			"command line is visible to every process on the machine and is kept in shell history.\n\n" +
			"Adding an instance does not contact it. Use `instance health` afterwards, which is also\n" +
			"what tells you whether the credentials work.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			password, err := readPassword(cmd, passwordStdin)
			if err != nil {
				return err
			}
			parsedLabels, err := parseLabels(labels)
			if err != nil {
				return err
			}

			c := newClient(*serverURL, *timeout)
			env, err := resolveEnvironment(ctx, c, environmentName)
			if err != nil {
				return err
			}

			connection := map[string]any{
				"username": username,
				"password": password,
				"database": database,
			}
			if tlsEnabled {
				connection["tls"] = map[string]any{
					"enabled":              true,
					"insecure_skip_verify": tlsSkipVerify,
				}
			}

			body := map[string]any{
				"environment_id": env.ID,
				"name":           args[0],
				"engine_type":    engineType,
				"host":           host,
				"port":           port,
				"connection":     connection,
				"labels":         parsedLabels,
			}

			var resp struct {
				Instance instance `json:"instance"`
			}
			if err := c.post(ctx, "/api/v1/instances", body, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "instance %q added to %s (%s)\n",
				resp.Instance.Name, env.Name, resp.Instance.ID)
			fmt.Fprintf(out, "check it with: fleetward-cli instance health %s\n", resp.Instance.Name)
			if tlsEnabled && tlsSkipVerify {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"warning: TLS certificate verification is disabled for this connection")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&environmentName, "environment", "", "environment name or identifier (required)")
	cmd.Flags().StringVar(&engineType, "engine", "", "engine type, e.g. postgresql (required)")
	cmd.Flags().StringVar(&host, "host", "", "hostname or address (required)")
	cmd.Flags().Int32Var(&port, "port", 0, "port (required)")
	cmd.Flags().StringVar(&username, "username", "", "database user Fleetward connects as (required)")
	cmd.Flags().StringVar(&database, "database", "", "default database to connect to")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from standard input")
	cmd.Flags().BoolVar(&tlsEnabled, "tls", false, "connect with TLS")
	cmd.Flags().BoolVar(&tlsSkipVerify, "tls-insecure-skip-verify", false,
		"skip certificate verification; development only")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "label as key=value; repeatable")

	for _, required := range []string{"environment", "engine", "host", "port", "username"} {
		_ = cmd.MarkFlagRequired(required)
	}
	return cmd
}

func newInstanceListCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var environmentName, engineType string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			query := url.Values{}
			if environmentName != "" {
				env, err := resolveEnvironment(ctx, c, environmentName)
				if err != nil {
					return err
				}
				query.Set("environment_id", env.ID)
			}
			if engineType != "" {
				query.Set("engine_type", engineType)
			}

			instances, err := listInstances(ctx, c, query)
			if err != nil {
				return err
			}
			if len(instances) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No instances yet. Add one with `fleetward-cli instance add --help`.")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tENGINE\tENDPOINT\tHEALTH\tVERSION\tLAST SEEN")
			for _, inst := range instances {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					inst.Name,
					inst.EngineType,
					inst.endpoint(),
					trimEnum("HEALTH_STATE_", inst.Health),
					orDash(inst.EngineVersion),
					formatLastSeen(inst.LastSeenAt))
			}
			return tw.Flush()
		},
	}

	cmd.Flags().StringVar(&environmentName, "environment", "", "only instances in this environment")
	cmd.Flags().StringVar(&engineType, "engine", "", "only instances of this engine type")
	return cmd
}

func newInstanceGetCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "Show one instance in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			inst, err := resolveInstance(ctx, c, args[0])
			if err != nil {
				return err
			}

			var resp struct {
				Instance  instance       `json:"instance"`
				Server    *serverInfo    `json:"server"`
				Databases []databaseInfo `json:"databases"`
				Topology  *topology      `json:"topology"`
			}
			if err := c.get(ctx, "/api/v1/instances/"+inst.ID, nil, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n", resp.Instance.Name)
			fmt.Fprintf(out, "  id         %s\n", resp.Instance.ID)
			fmt.Fprintf(out, "  engine     %s %s\n", resp.Instance.EngineType, resp.Instance.EngineVersion)
			fmt.Fprintf(out, "  endpoint   %s\n", resp.Instance.endpoint())
			fmt.Fprintf(out, "  health     %s\n", trimEnum("HEALTH_STATE_", resp.Instance.Health))
			fmt.Fprintf(out, "  last seen  %s\n", formatLastSeen(resp.Instance.LastSeenAt))
			for key, value := range resp.Instance.Labels {
				fmt.Fprintf(out, "  label      %s=%s\n", key, value)
			}

			if resp.Server == nil && len(resp.Databases) == 0 {
				fmt.Fprintf(out, "\nNo discovery on record. Run: fleetward-cli instance discover %s\n",
					resp.Instance.Name)
				return nil
			}
			printDiscovery(out, resp.Server, resp.Databases, resp.Topology)
			return nil
		},
	}
}

func newInstanceHealthCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	return &cobra.Command{
		Use:   "health <name-or-id>",
		Short: "Check whether an instance is reachable and healthy",
		Long: "Resolves the stored credentials, probes the instance through its engine plugin, and\n" +
			"records the result. Exits non-zero when the instance is not up, so it can be scripted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			inst, err := resolveInstance(ctx, c, args[0])
			if err != nil {
				return err
			}

			var resp struct {
				Success bool         `json:"success"`
				Health  healthStatus `json:"health"`
				Message string       `json:"message"`
			}
			path := "/api/v1/instances/" + inst.ID + "/test-connection"
			if err := c.post(ctx, path, map[string]any{}, &resp); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			state := trimEnum("HEALTH_STATE_", resp.Health.State)
			fmt.Fprintf(out, "%s  %s\n", inst.Name, state)
			fmt.Fprintf(out, "  endpoint   %s\n", inst.endpoint())
			if resp.Health.EngineVersion != "" {
				fmt.Fprintf(out, "  version    %s\n", resp.Health.EngineVersion)
			}
			if resp.Health.Latency != "" {
				fmt.Fprintf(out, "  latency    %s\n", formatDuration(resp.Health.Latency))
			}
			if resp.Message != "" {
				fmt.Fprintf(out, "  message    %s\n", resp.Message)
			}
			for _, signal := range resp.Health.Signals {
				fmt.Fprintf(out, "  signal     %-22s %s %g%s %s\n",
					signal.Name,
					trimEnum("SEVERITY_", signal.Severity),
					signal.Value, signal.Unit,
					signal.Message)
			}
			if resp.Health.Error != nil {
				fmt.Fprintf(out, "  error      %s: %s (retryable: %s)\n",
					trimEnum("ERROR_CODE_", resp.Health.Error.Code),
					resp.Health.Error.Message,
					yesNo(resp.Health.Error.Retryable))
			}

			if !resp.Success {
				return fmt.Errorf("instance %q is %s", inst.Name, state)
			}
			return nil
		},
	}
}

func newInstanceDiscoverCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	return &cobra.Command{
		Use:   "discover <name-or-id>",
		Short: "Refresh topology, version, and databases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			inst, err := resolveInstance(ctx, c, args[0])
			if err != nil {
				return err
			}

			var resp struct {
				Server    *serverInfo    `json:"server"`
				Databases []databaseInfo `json:"databases"`
				Topology  *topology      `json:"topology"`
			}
			path := "/api/v1/instances/" + inst.ID + "/discover"
			if err := c.post(ctx, path, map[string]any{}, &resp); err != nil {
				return err
			}

			printDiscovery(cmd.OutOrStdout(), resp.Server, resp.Databases, resp.Topology)
			return nil
		},
	}
}

func newInstanceRemoveCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	var confirmed bool

	cmd := &cobra.Command{
		Use:     "remove <name-or-id>",
		Aliases: []string{"rm"},
		Short:   "Remove an instance from the inventory",
		Long: "Removes the instance, its connections, and its stored credentials.\n\n" +
			"Backup artifacts are left in place: removing a server from the inventory must not\n" +
			"silently destroy the backups taken from it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			c := newClient(*serverURL, *timeout)
			inst, err := resolveInstance(ctx, c, args[0])
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("refusing to remove %q without --yes", inst.Name)
			}

			if err := c.delete(ctx, "/api/v1/instances/"+inst.ID, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "instance %q removed; its backups were left in place\n", inst.Name)
			return nil
		},
	}

	cmd.Flags().BoolVar(&confirmed, "yes", false, "confirm removal")
	return cmd
}

// -----------------------------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------------------------

// listInstances walks every page.
func listInstances(ctx context.Context, c *client, query url.Values) ([]instance, error) {
	if query == nil {
		query = url.Values{}
	}

	var all []instance
	pageToken := ""
	for {
		page := url.Values{}
		for key, values := range query {
			page[key] = values
		}
		if pageToken != "" {
			page.Set("page_token", pageToken)
		}

		var resp struct {
			Instances     []instance `json:"instances"`
			NextPageToken string     `json:"next_page_token"`
		}
		if err := c.get(ctx, "/api/v1/instances", page, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Instances...)
		if resp.NextPageToken == "" {
			return all, nil
		}
		pageToken = resp.NextPageToken
	}
}

// resolveInstance accepts an identifier or a name.
//
// A name is unique per environment rather than per estate, so the same name in two environments is
// legitimate; the ambiguity is reported instead of being resolved arbitrarily.
func resolveInstance(ctx context.Context, c *client, nameOrID string) (*instance, error) {
	instances, err := listInstances(ctx, c, nil)
	if err != nil {
		return nil, err
	}

	var matches []instance
	for _, inst := range instances {
		if inst.ID == nameOrID || strings.EqualFold(inst.Name, nameOrID) {
			matches = append(matches, inst)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no instance named %q; `fleetward-cli instance list` shows what exists", nameOrID)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%q matches %d instances in different environments; use the identifier instead",
			nameOrID, len(matches))
	}
}

// readPassword takes the password from stdin or the environment, never from a flag.
func readPassword(cmd *cobra.Command, fromStdin bool) (string, error) {
	if fromStdin {
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		// Trailing newlines come from `echo` and from here-strings, and a password with one
		// appended fails authentication in a way that is very hard to see.
		return strings.TrimRight(string(raw), "\r\n"), nil
	}

	// LookupEnv rather than Getenv so an explicitly empty variable means "this instance has no
	// password", which is a real configuration for trust and socket authentication.
	if value, ok := os.LookupEnv(passwordEnvVar); ok {
		return value, nil
	}
	return "", errors.New("no password supplied: set " + passwordEnvVar + " or pass --password-stdin")
}

// parseLabels turns repeated key=value flags into a map.
func parseLabels(raw []string) (map[string]string, error) {
	labels := make(map[string]string, len(raw))
	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("label %q is not in key=value form", entry)
		}
		labels[key] = value
	}
	return labels, nil
}

func printDiscovery(out io.Writer, server *serverInfo, databases []databaseInfo, topo *topology) {
	if server != nil {
		fmt.Fprintf(out, "\nserver\n")
		fmt.Fprintf(out, "  version    %s (%s)\n", server.Version, server.VersionString)
		if server.Uptime != "" {
			fmt.Fprintf(out, "  uptime     %s\n", formatDuration(server.Uptime))
		}
		fmt.Fprintf(out, "  read only  %s\n", yesNo(server.ReadOnly))
	}

	if len(databases) > 0 {
		fmt.Fprintf(out, "\ndatabases\n")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tSIZE\tOWNER\tOBJECTS\tSYSTEM")
		for _, db := range databases {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
				db.Name, orDash(db.SizeBytes), orDash(db.Owner), orDash(db.ObjectCount), yesNo(db.IsSystem))
		}
		_ = tw.Flush()
	}

	if topo != nil && len(topo.Nodes) > 0 {
		fmt.Fprintf(out, "\ntopology (standalone: %s)\n", yesNo(topo.IsStandalone))
		for _, n := range topo.Nodes {
			marker := " "
			if n.IsSelf {
				marker = "*"
			}
			fmt.Fprintf(out, "  %s %s:%d  %s  %s\n",
				marker, n.Host, n.Port, trimEnum("NODE_ROLE_", n.Role), n.State)
		}
	}
}

// formatDuration renders a protobuf duration for a human.
//
// Protojson writes them as a bare number of seconds — "0.012897666s" — which is unreadable at the
// scale a health probe operates on. Anything that does not parse is shown as it arrived rather than
// swallowed.
func formatDuration(raw string) string {
	seconds, err := strconv.ParseFloat(strings.TrimSuffix(raw, "s"), 64)
	if err != nil {
		return raw
	}
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

func formatLastSeen(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "never"
	}
	return at.Local().Format(time.RFC3339)
}

func orDash(value string) string {
	if value == "" || value == "0" {
		return "-"
	}
	return value
}
