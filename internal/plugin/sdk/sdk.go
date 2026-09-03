// Package sdk is the harness engine plugin authors implement against (ADR-0003).
//
// A plugin is a standalone binary. Its main function is normally three lines:
//
//	func main() {
//		sdk.Serve(myengine.New())
//	}
//
// Everything else — the go-plugin handshake, the gRPC server, stream plumbing, panic recovery — is
// handled here so that plugin authors write engine logic and nothing else.
//
// The contract itself lives in api/proto/fleetward/v1/plugin.proto. This package deliberately
// exposes the generated protobuf types rather than a parallel set of Go structs: one definition of
// the contract means one place for it to be wrong, and a plugin author reading the proto sees
// exactly the types they will implement against.
package sdk

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Emitter delivers one message of a streaming response. Returning an error means the stream has
// failed — normally because the caller went away — and the engine should abandon its work.
type Emitter[T any] func(T) error

// Engine is the complete set of behavior an engine plugin provides. It mirrors the EnginePlugin
// gRPC service, with server streams expressed as an Emitter so implementations are ordinary
// straight-line Go.
//
// Embed Base to inherit "not supported" implementations of everything, then override only what the
// engine actually does. A plugin that declares supports_pitr = false has no reason to write a
// ListPITRTargets method.
type Engine interface {
	// Capabilities reports the feature matrix. It must not require a database connection, and it
	// must be honest: core trusts this matrix when deciding what is safe to do to a production
	// database.
	Capabilities(ctx context.Context) (*fwv1.Capabilities, error)

	Discover(ctx context.Context, req *fwv1.DiscoverRequest) (*fwv1.DiscoverResponse, error)
	GetConfig(ctx context.Context, req *fwv1.GetConfigRequest) (*fwv1.GetConfigResponse, error)

	// CollectMetrics emits one or more batches for a single collection cycle, then returns.
	CollectMetrics(ctx context.Context, req *fwv1.CollectMetricsRequest, emit Emitter[*fwv1.MetricBatch]) error

	// Backup emits progress as it runs. The final emitted message must carry either a
	// BackupResult with phase JOB_PHASE_COMPLETED or a PluginError with phase JOB_PHASE_FAILED;
	// returning without emitting a terminal message is a contract violation the conformance suite
	// checks for.
	Backup(ctx context.Context, req *fwv1.BackupRequest, emit Emitter[*fwv1.BackupProgress]) error

	// Restore emits progress as it runs, with the same terminal-message rule as Backup.
	Restore(ctx context.Context, req *fwv1.RestoreRequest, emit Emitter[*fwv1.RestoreProgress]) error

	// VerifyRestore smoke-tests an instance that Restore has just populated.
	VerifyRestore(ctx context.Context, req *fwv1.VerifyRestoreRequest) (*fwv1.VerifyRestoreResult, error)

	ListPITRTargets(ctx context.Context, req *fwv1.ListPITRTargetsRequest) (*fwv1.PITRWindow, error)

	// ListBackupHistory reports backups the engine knows about that Fleetward did not take
	// (ADR-0015). Implementations must be read-only and must honour req.Limit: an engine's own
	// backup history can hold hundreds of thousands of rows on an instance that has been up for
	// years, and scanning all of them on every poll is not acceptable against a production server.
	ListBackupHistory(ctx context.Context, req *fwv1.ListBackupHistoryRequest) (*fwv1.ListBackupHistoryResponse, error)

	// ListPrincipals enumerates users, roles, and privileges. Implementations must be read-only.
	ListPrincipals(ctx context.Context, req *fwv1.ListPrincipalsRequest) (*fwv1.ListPrincipalsResponse, error)

	// HealthCheck reports liveness. An unreachable instance is a HealthStatus with
	// HEALTH_STATE_DOWN, not an error: being down is a valid answer.
	HealthCheck(ctx context.Context, req *fwv1.HealthCheckRequest) (*fwv1.HealthStatus, error)
}
