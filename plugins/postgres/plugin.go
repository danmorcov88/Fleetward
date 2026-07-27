// Package postgres implements the Fleetward engine plugin for PostgreSQL.
//
// This is the reference plugin. It is built first and in the most depth, and the conformance suite
// is written against it (ADR-0012); the other three engines then run that same suite unmodified,
// which is how contract leaks get found.
//
// Stage 0 status: the plugin handshakes and reports its identity. Every capability flag is false
// and no backup methods are declared, because none are implemented yet. Capabilities are a promise
// core relies on when deciding what to do to a production database, so they are turned on in the
// same change that implements them — never before.
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

		// Stage 1 turns these on alongside the implementations behind them:
		//   SupportsPitr, SupportsSchemaDiscovery, SupportsConfigRead, SupportsReplication,
		//   SupportsReplicationLag, SupportsStorageMetrics, SupportsOnlineBackup,
		//   SupportsSandboxRestore, SupportsPointInTimeRestore,
		//   BackupMethods (pg_basebackup + WAL archiving, with pgbackrest as a later method),
		//   PrincipalModel PRINCIPAL_MODEL_USERS_AND_ROLES,
		//   SandboxTemplate (postgres image, pg_isready readiness probe).

		Metadata: map[string]string{
			"stage": "0 — handshake only",
		},
	}, nil
}
