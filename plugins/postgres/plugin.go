// Package postgres implements the Fleetward engine plugin for PostgreSQL.
//
// This is the reference plugin. It is built first and in the most depth, and the conformance suite
// is written against it (ADR-0012); the other three engines then run that same suite unmodified,
// which is how contract leaks get found.
//
// Slice A5 status: HealthCheck, Discover, Backup, Restore, and VerifyRestore are implemented
// against a real instance. Only
// the capabilities those RPCs deliver are declared. Capabilities are a promise core relies on when
// deciding what to do to a production database, so they are turned on in the same change that
// implements them — never before.
package postgres

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

// EngineType is the canonical identifier for this engine. The plugin binary must be named
// fleetward-plugin-<EngineType> so the manager's handshake check passes.
const EngineType = "postgresql"

// Plugin is the PostgreSQL engine plugin.
//
// Embedding sdk.Base supplies "not supported" implementations of everything not yet written, so
// the plugin satisfies the contract at every stage of its construction rather than only at the end.
type Plugin struct {
	sdk.Base
}

var _ sdk.Engine = (*Plugin)(nil)

// New builds the plugin.
func New() *Plugin {
	return &Plugin{Base: sdk.Base{EngineType: EngineType}}
}

// Capabilities reports what this plugin can do today.
func (p *Plugin) Capabilities(context.Context) (*fwv1.Capabilities, error) {
	return &fwv1.Capabilities{
		EngineType:              EngineType,
		EngineDisplayName:       "PostgreSQL",
		PluginVersion:           version.Version,
		ContractVersion:         version.ContractVersion,
		SupportedEngineVersions: []string{">=13 <18"},

		// Delivered by Discover: databases with sizes and owners, and the replication layout as
		// seen from this node.
		SupportsSchemaDiscovery: true,
		SupportsReplication:     true,
		// Delivered by HealthCheck: replay lag is reported for a standby, and core gates its lag
		// health rule and the UI's lag column on this flag.
		SupportsReplicationLag: true,

		// Delivered by Backup: pg_dump runs against a live server without blocking writers, from an
		// exported snapshot that the manifest is counted in.
		SupportsOnlineBackup: true,
		BackupMethods:        backupMethods(),
		// Declared at the plugin level as well as on the method, so the manager reports a host
		// without them as a plugin health problem rather than letting the first scheduled backup
		// discover it at 3am — or, worse, letting a verification discover it during an incident.
		RequiredTools: []string{dumpTool, restoreTool, psqlTool},

		// Delivered by Restore and VerifyRestore: an artifact is loaded into a throwaway container
		// of the version that produced it and compared against the manifest taken with it.
		SupportsSandboxRestore:      true,
		SandboxTemplate:             sandboxTemplate(),
		SupportedVerificationChecks: supportedChecks(),

		// Not yet implemented, so not yet declared. Each is turned on by the slice that builds it:
		//   B   SupportsPitr, SupportsPointInTimeRestore, the pg_basebackup method
		//   C   PrincipalModel PRINCIPAL_MODEL_USERS_AND_ROLES
		//   F   SupportsConfigRead, SupportsStorageMetrics, Metrics

		Metadata: map[string]string{
			"slice": "A5 — restore into a sandbox and verify",
		},
	}, nil
}
