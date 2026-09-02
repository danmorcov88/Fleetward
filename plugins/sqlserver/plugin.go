// Package sqlserver implements the Fleetward engine plugin for Microsoft SQL Server.
//
// It is the second engine, and its real job is to test a claim the architecture has been making
// since before there was anything to test it with: that adding an engine never means modifying
// core. PostgreSQL cannot test that claim, because the contract was written against it.
//
// SQL Server asks the contract a question PostgreSQL never did. pg_dump writes to stdout, and the
// plugin streams that into presigned part grants. BACKUP DATABASE writes a file on the database
// server's own filesystem, where the plugin has no access at all — so the artifact is handed over
// through a directory both of them can see (ADR-0026), and the upload from there is the same code
// PostgreSQL uses.
//
// Slice B2 status: HealthCheck, Discover, Backup, Restore, and VerifyRestore are implemented
// against a real instance. Only the capabilities those RPCs deliver are declared. Capabilities are
// a promise core relies on when deciding what to do to a production database, so each is turned on
// in the same change that implements it — never before.
package sqlserver

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/version"
)

// EngineType is the canonical identifier for this engine. The plugin binary must be named
// fleetward-plugin-<EngineType> so the manager's handshake check passes.
const EngineType = "sqlserver"

// Plugin is the SQL Server engine plugin.
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
		EngineType:        EngineType,
		EngineDisplayName: "Microsoft SQL Server",
		PluginVersion:     version.Version,
		ContractVersion:   version.ContractVersion,
		// 2016 is where the T-SQL this plugin relies on settles down; 2022 is the newest it has
		// been exercised against. The upper bound is a statement about testing, not about syntax.
		SupportedEngineVersions: []string{">=13 <17"},

		// Delivered by Discover: databases with sizes and recovery models, and the instance's own
		// version and edition.
		SupportsSchemaDiscovery: true,

		// Delivered by Backup: BACKUP DATABASE runs against a live server without blocking
		// writers, and its artifact is consistent at the point the backup ends.
		SupportsOnlineBackup: true,
		BackupMethods:        backupMethods(),
		// Deliberately empty. BACKUP and RESTORE are T-SQL statements rather than command-line
		// tools, so this plugin shells out to nothing: no client package to install, no PATH to get
		// wrong, and no tool exit code for core to misread as evidence about an artifact.
		RequiredTools: nil,

		// Delivered by Restore and VerifyRestore: an artifact is loaded into a throwaway container
		// and compared against the manifest taken with it.
		SupportsSandboxRestore:      true,
		SandboxTemplate:             sandboxTemplate(),
		SupportedVerificationChecks: supportedChecks(),

		// Not yet implemented, so not yet declared. Each is turned on by the slice that builds it:
		//   B3   ListBackupHistory over msdb.dbo.backupset — the richest such source of any engine
		//   B12+ SupportsPitr and SupportsPointInTimeRestore, on log backups
		//   later PrincipalModel, SupportsConfigRead, SupportsStorageMetrics, Metrics

		Metadata: map[string]string{
			"slice": "B2 — the SQL Server plugin",
		},
	}, nil
}

// supportedChecks is what VerifyRestore actually runs. The conformance suite fails a plugin that
// declares a check it does not run, because a green verification covering less than it claims is
// worse than one that admits its scope.
func supportedChecks() []fwv1.VerificationCheck {
	return []fwv1.VerificationCheck{
		fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
		fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
		fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY,
		fwv1.VerificationCheck_VERIFICATION_CHECK_QUERYABILITY,
	}
}
