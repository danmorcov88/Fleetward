// Command fleetward-cli is the Fleetward command-line client.
//
// `version` and `keygen` need no server. Everything else goes through the control plane's REST API,
// never directly to the metadata store: a CLI holding the metadata database's password would put it
// on every operator's laptop and duplicate authorization in a second place.
//
// Today that is `health`, `environment`, `instance`, `backup`, `schedule`, and `job`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/danmorcov88/fleetward/internal/storage/secrets"
	"github.com/danmorcov88/fleetward/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		// Cobra has already printed the error; exiting non-zero is all that remains.
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var (
		serverURL string
		timeout   time.Duration
	)

	root := &cobra.Command{
		Use:   "fleetward",
		Short: "Fleetward — multi-engine DBA operations control plane",
		Long: "Fleetward is a multi-engine DBA operations control plane: estate inventory, health\n" +
			"monitoring, backups with automated restore verification, access visibility, and alerting.",
		SilenceUsage: true,
		// Cobra prints usage on any error by default, which buries a real failure under a wall of
		// flags. Errors are printed by the command that produced them.
		SilenceErrors: false,
	}

	root.PersistentFlags().StringVar(&serverURL, "server", envOr("FLEETWARD_SERVER", "http://localhost:8080"),
		"control plane base URL")
	// Every command that talks to the control plane is bounded by the same deadline. Probing an
	// estate means routinely contacting servers that will not answer, and a command that hangs is
	// worse than one that fails.
	root.PersistentFlags().DurationVar(&timeout, "timeout", 30*time.Second,
		"how long to wait for the control plane to respond")

	root.AddCommand(
		newVersionCommand(),
		newHealthCommand(&serverURL, &timeout),
		newKeygenCommand(),
		newEnvironmentCommand(&serverURL, &timeout),
		newInstanceCommand(&serverURL, &timeout),
		newBackupCommand(&serverURL, &timeout),
		newScheduleCommand(&serverURL, &timeout),
		newJobCommand(&serverURL, &timeout),
	)

	return root
}

func newVersionCommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "fleetward", info)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func newHealthCommand(serverURL *string, timeout *time.Duration) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check control plane readiness",
		Long: "Queries the control plane's /readyz endpoint and reports each dependency.\n" +
			"Exits non-zero when the control plane is unhealthy, so it can be used in scripts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, *serverURL+"/readyz", nil)
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("contact control plane at %s: %w", *serverURL, err)
			}
			defer func() { _ = resp.Body.Close() }()

			var status struct {
				Status     string `json:"status"`
				Components []struct {
					Name      string `json:"name"`
					Status    string `json:"status"`
					Critical  bool   `json:"critical"`
					Error     string `json:"error"`
					LatencyMS int64  `json:"latency_ms"`
				} `json:"components"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "control plane: %s\n", status.Status)
			for _, c := range status.Components {
				marker := "ok"
				if c.Status != "healthy" {
					marker = "FAIL"
				}
				fmt.Fprintf(out, "  %-12s %-5s %4dms", c.Name, marker, c.LatencyMS)
				if c.Error != "" {
					fmt.Fprintf(out, "  %s", c.Error)
				}
				fmt.Fprintln(out)
			}

			if status.Status == "unhealthy" {
				return fmt.Errorf("control plane is unhealthy")
			}
			return nil
		},
	}

	return cmd
}

func newKeygenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Generate a secrets master key",
		Long: "Generates a base64-encoded 32-byte AES-256 key for the aesgcm secrets provider.\n\n" +
			"Store it in a file and point FLEETWARD_SECRETS_MASTER_KEY_FILE at it. Prefer a file\n" +
			"over the environment variable: anything that can read the process environment can\n" +
			"read the key, and with it every stored database credential.\n\n" +
			"Losing this key makes every stored credential unrecoverable. Back it up somewhere\n" +
			"that is not the machine it protects.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := secrets.GenerateMasterKey()
			if err != nil {
				return err
			}
			// The key goes to stdout alone so it can be redirected into a file; the warning goes
			// to stderr so redirecting stdout does not swallow it.
			fmt.Fprintln(cmd.OutOrStdout(), key)
			fmt.Fprintln(cmd.ErrOrStderr(),
				"\nStore this key securely. If you lose it, every stored credential is unrecoverable.")
			return nil
		},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
