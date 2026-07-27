// Package mongodb implements the Fleetward engine plugin for MongoDB.
//
// Stage 0 status: the plugin handshakes and reports its identity. Every capability flag is false
// and no backup methods are declared, because none are implemented yet. Capabilities are a promise
// core relies on when deciding what to do to a production database, so they are turned on in the
// same change that implements them — never before.
//
// Stage 3 builds this plugin out against the conformance suite born with the PostgreSQL reference
// plugin. It runs that suite unmodified: if something has to change in the suite to accommodate
// this engine, that is a contract leak to fix in the contract, not in the test.
package mongodb

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

// EngineType is the canonical identifier for this engine. The plugin binary must be named
// fleetward-plugin-<EngineType> so the manager's handshake check passes.
const EngineType = "mongodb"

// Plugin is the MongoDB engine plugin.
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
		EngineDisplayName:       "MongoDB",
		PluginVersion:           version.Version,
		ContractVersion:         version.ContractVersion,
		SupportedEngineVersions: []string{">=6.0"},

		// Stage 3 turns these on alongside the implementations behind them:
		//   BackupMethods: mongodump as method #1. Snapshot-based backup is documented as the
		//   follow-on path and will arrive as an additional method, not a replacement.
		//   PrincipalModel PRINCIPAL_MODEL_USERS_AND_ROLES.
		//   SandboxTemplate (mongo image, mongosh ping readiness probe).

		Metadata: map[string]string{
			"stage": "0 — handshake only",
		},
	}, nil
}
