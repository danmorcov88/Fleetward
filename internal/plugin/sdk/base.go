package sdk

import (
	"context"
	"fmt"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Base provides "not supported" implementations of every optional Engine method.
//
// Embedding it means a new plugin can start by implementing only Capabilities and HealthCheck and
// still satisfy the interface — which is exactly the path acceptance criterion 8 describes: a
// developer scaffolds a fifth engine, passes the capabilities and health subset of conformance,
// and fills in the rest incrementally, without touching core at any point.
//
// Every unimplemented method returns an ERROR_CODE_UNSUPPORTED PluginError. Core distinguishes
// that from a genuine failure, so an engine that cannot do point-in-time recovery is reported as
// "PITR not available for this engine" rather than "PITR is broken".
type Base struct {
	// EngineType is used in the "not supported" messages so that a log line identifies which
	// plugin declined.
	EngineType string
}

func (b Base) unsupported(rpc string) error {
	engine := b.EngineType
	if engine == "" {
		engine = "this engine"
	}
	return Unsupported("%s does not implement %s", engine, rpc)
}

// Capabilities must be implemented by every plugin; there is no sensible default.
func (b Base) Capabilities(context.Context) (*fwv1.Capabilities, error) {
	return nil, b.unsupported("GetCapabilities")
}

// Discover implements Engine.
func (b Base) Discover(context.Context, *fwv1.DiscoverRequest) (*fwv1.DiscoverResponse, error) {
	return nil, b.unsupported("Discover")
}

// GetConfig implements Engine.
func (b Base) GetConfig(context.Context, *fwv1.GetConfigRequest) (*fwv1.GetConfigResponse, error) {
	return nil, b.unsupported("GetConfig")
}

// CollectMetrics implements Engine.
func (b Base) CollectMetrics(context.Context, *fwv1.CollectMetricsRequest, Emitter[*fwv1.MetricBatch]) error {
	return b.unsupported("CollectMetrics")
}

// Backup implements Engine.
func (b Base) Backup(context.Context, *fwv1.BackupRequest, Emitter[*fwv1.BackupProgress]) error {
	return b.unsupported("Backup")
}

// Restore implements Engine.
func (b Base) Restore(context.Context, *fwv1.RestoreRequest, Emitter[*fwv1.RestoreProgress]) error {
	return b.unsupported("Restore")
}

// VerifyRestore implements Engine.
func (b Base) VerifyRestore(context.Context, *fwv1.VerifyRestoreRequest) (*fwv1.VerifyRestoreResult, error) {
	return nil, b.unsupported("VerifyRestore")
}

// ListPITRTargets implements Engine. Engines without point-in-time recovery return an unavailable
// window rather than an error, so the UI can explain the absence instead of showing a failure.
func (b Base) ListPITRTargets(context.Context, *fwv1.ListPITRTargetsRequest) (*fwv1.PITRWindow, error) {
	engine := b.EngineType
	if engine == "" {
		engine = "this engine"
	}
	return &fwv1.PITRWindow{
		Available:         false,
		UnavailableReason: fmt.Sprintf("point-in-time recovery is not supported by %s", engine),
	}, nil
}

// ListBackupHistory implements Engine. A plugin that cannot see backups it did not take refuses
// rather than answering with an empty list: "there is no evidence here" and "there were no backups"
// are different statements, and rendering the first as the second would report a healthy estate for
// an engine nobody is watching.
func (b Base) ListBackupHistory(context.Context, *fwv1.ListBackupHistoryRequest) (*fwv1.ListBackupHistoryResponse, error) {
	return nil, b.unsupported("ListBackupHistory")
}

// ListPrincipals implements Engine.
func (b Base) ListPrincipals(context.Context, *fwv1.ListPrincipalsRequest) (*fwv1.ListPrincipalsResponse, error) {
	return &fwv1.ListPrincipalsResponse{Model: fwv1.PrincipalModel_PRINCIPAL_MODEL_NONE}, nil
}

// HealthCheck implements Engine.
func (b Base) HealthCheck(context.Context, *fwv1.HealthCheckRequest) (*fwv1.HealthStatus, error) {
	return nil, b.unsupported("HealthCheck")
}
