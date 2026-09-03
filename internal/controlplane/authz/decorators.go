package authz

import (
	"context"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// The decorators.
//
// Each one wraps a generated service interface and calls the guard before delegating. What matters
// is what happens to an RPC somebody adds to the contract and forgets to decorate, and there are
// three answers, none of which is "it reaches the service unguarded".
//
// **The embedded Unimplemented is the fail-closed default.** `protoc-gen-go-grpc` generates its
// service interfaces with `require_unimplemented_servers` on, so every implementation must embed
// `UnimplementedXServiceServer` — this decorator included. The brief for this slice wanted the
// opposite: a decorator that omitted the embed and therefore stopped *compiling* when the contract
// grew a method, the way B5's CHECK constraint refuses a bad state. That is not available without
// turning the option off globally, and turning it off globally would make every additive change to
// `plugin.proto` break every third-party plugin at compile time — which is precisely the forward
// compatibility CONTRIBUTING.md promises plugin authors.
//
// So the embed stays, and it is not a silent hole: a method this decorator does not override is
// answered by the embedded Unimplemented with codes.Unimplemented. The request is refused, and the
// real service behind the decorator is never reached. Loud, and closed.
//
// **The guard is the second barrier.** A method with no entry in Policies is denied to everybody,
// including an administrator, until somebody writes down what it needs.
//
// **The coverage test is the third**, and it is what turns "loud at runtime" back into "loud in
// CI". It enumerates every method on every generated service interface by reflection and calls each
// one with an anonymous caller, asserting codes.Unauthenticated. A method that was forgotten here
// answers codes.Unimplemented instead, so the same test that proves every route requires a
// credential also proves every route is decorated. See coverage_test.go.

// -----------------------------------------------------------------------------------------------
// Inventory
// -----------------------------------------------------------------------------------------------

type inventoryGuard struct {
	// Required by the generated interface, and load-bearing: it is what answers a method this
	// decorator forgot to override, with Unimplemented rather than with the real service.
	fwv1.UnimplementedInventoryServiceServer

	e     *Enforcer
	inner fwv1.InventoryServiceServer
}

// GuardInventory wraps the inventory service.
func GuardInventory(e *Enforcer, inner fwv1.InventoryServiceServer) fwv1.InventoryServiceServer {
	return &inventoryGuard{e: e, inner: inner}
}

var _ fwv1.InventoryServiceServer = (*inventoryGuard)(nil)

func (g *inventoryGuard) ListEnvironments(ctx context.Context, req *fwv1.ListEnvironmentsRequest) (*fwv1.ListEnvironmentsResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/ListEnvironments", req, g.inner.ListEnvironments)
}

func (g *inventoryGuard) CreateEnvironment(ctx context.Context, req *fwv1.CreateEnvironmentRequest) (*fwv1.CreateEnvironmentResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/CreateEnvironment", req, g.inner.CreateEnvironment)
}

func (g *inventoryGuard) ListInstances(ctx context.Context, req *fwv1.ListInstancesRequest) (*fwv1.ListInstancesResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/ListInstances", req, g.inner.ListInstances)
}

func (g *inventoryGuard) GetInstance(ctx context.Context, req *fwv1.GetInstanceRequest) (*fwv1.GetInstanceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/GetInstance", req, g.inner.GetInstance)
}

func (g *inventoryGuard) CreateInstance(ctx context.Context, req *fwv1.CreateInstanceRequest) (*fwv1.CreateInstanceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/CreateInstance", req, g.inner.CreateInstance)
}

func (g *inventoryGuard) DeleteInstance(ctx context.Context, req *fwv1.DeleteInstanceRequest) (*fwv1.DeleteInstanceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/DeleteInstance", req, g.inner.DeleteInstance)
}

func (g *inventoryGuard) TestConnection(ctx context.Context, req *fwv1.TestConnectionRequest) (*fwv1.TestConnectionResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/TestConnection", req, g.inner.TestConnection)
}

func (g *inventoryGuard) DiscoverInstance(ctx context.Context, req *fwv1.DiscoverInstanceRequest) (*fwv1.DiscoverInstanceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/DiscoverInstance", req, g.inner.DiscoverInstance)
}

func (g *inventoryGuard) ListPrincipalsForInstance(ctx context.Context, req *fwv1.ListPrincipalsForInstanceRequest) (*fwv1.ListPrincipalsForInstanceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.InventoryService/ListPrincipalsForInstance", req, g.inner.ListPrincipalsForInstance)
}

// -----------------------------------------------------------------------------------------------
// Schedules
// -----------------------------------------------------------------------------------------------

type scheduleGuard struct {
	// Required by the generated interface, and load-bearing: it is what answers a method this
	// decorator forgot to override, with Unimplemented rather than with the real service.
	fwv1.UnimplementedScheduleServiceServer

	e     *Enforcer
	inner fwv1.ScheduleServiceServer
}

// GuardSchedule wraps the schedule service.
func GuardSchedule(e *Enforcer, inner fwv1.ScheduleServiceServer) fwv1.ScheduleServiceServer {
	return &scheduleGuard{e: e, inner: inner}
}

var _ fwv1.ScheduleServiceServer = (*scheduleGuard)(nil)

func (g *scheduleGuard) ListSchedules(ctx context.Context, req *fwv1.ListSchedulesRequest) (*fwv1.ListSchedulesResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/ListSchedules", req, g.inner.ListSchedules)
}

func (g *scheduleGuard) GetSchedule(ctx context.Context, req *fwv1.GetScheduleRequest) (*fwv1.GetScheduleResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/GetSchedule", req, g.inner.GetSchedule)
}

func (g *scheduleGuard) CreateSchedule(ctx context.Context, req *fwv1.CreateScheduleRequest) (*fwv1.CreateScheduleResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/CreateSchedule", req, g.inner.CreateSchedule)
}

func (g *scheduleGuard) SetScheduleEnabled(ctx context.Context, req *fwv1.SetScheduleEnabledRequest) (*fwv1.SetScheduleEnabledResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/SetScheduleEnabled", req, g.inner.SetScheduleEnabled)
}

func (g *scheduleGuard) DeleteSchedule(ctx context.Context, req *fwv1.DeleteScheduleRequest) (*fwv1.DeleteScheduleResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/DeleteSchedule", req, g.inner.DeleteSchedule)
}

func (g *scheduleGuard) ListJobs(ctx context.Context, req *fwv1.ListJobsRequest) (*fwv1.ListJobsResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.ScheduleService/ListJobs", req, g.inner.ListJobs)
}

// -----------------------------------------------------------------------------------------------
// Backups
// -----------------------------------------------------------------------------------------------

type backupGuard struct {
	// Required by the generated interface, and load-bearing: it is what answers a method this
	// decorator forgot to override, with Unimplemented rather than with the real service.
	fwv1.UnimplementedBackupServiceServer

	e     *Enforcer
	inner fwv1.BackupServiceServer
}

// GuardBackup wraps the backup service.
func GuardBackup(e *Enforcer, inner fwv1.BackupServiceServer) fwv1.BackupServiceServer {
	return &backupGuard{e: e, inner: inner}
}

var _ fwv1.BackupServiceServer = (*backupGuard)(nil)

func (g *backupGuard) ListBackups(ctx context.Context, req *fwv1.ListBackupsRequest) (*fwv1.ListBackupsResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/ListBackups", req, g.inner.ListBackups)
}

func (g *backupGuard) GetBackup(ctx context.Context, req *fwv1.GetBackupRequest) (*fwv1.GetBackupResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/GetBackup", req, g.inner.GetBackup)
}

func (g *backupGuard) RunBackup(ctx context.Context, req *fwv1.RunBackupRequest) (*fwv1.RunBackupResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/RunBackup", req, g.inner.RunBackup)
}

func (g *backupGuard) RunVerification(ctx context.Context, req *fwv1.RunVerificationRequest) (*fwv1.RunVerificationResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/RunVerification", req, g.inner.RunVerification)
}

func (g *backupGuard) GetVerification(ctx context.Context, req *fwv1.GetVerificationRequest) (*fwv1.GetVerificationResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/GetVerification", req, g.inner.GetVerification)
}

func (g *backupGuard) GetPITRWindow(ctx context.Context, req *fwv1.GetPITRWindowRequest) (*fwv1.GetPITRWindowResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/GetPITRWindow", req, g.inner.GetPITRWindow)
}

func (g *backupGuard) ObserveBackupHistory(ctx context.Context, req *fwv1.ObserveBackupHistoryRequest) (*fwv1.ObserveBackupHistoryResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/ObserveBackupHistory", req, g.inner.ObserveBackupHistory)
}

func (g *backupGuard) GetBackupAdherence(ctx context.Context, req *fwv1.GetBackupAdherenceRequest) (*fwv1.GetBackupAdherenceResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/GetBackupAdherence", req, g.inner.GetBackupAdherence)
}

func (g *backupGuard) PreviewRetention(ctx context.Context, req *fwv1.PreviewRetentionRequest) (*fwv1.PreviewRetentionResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.BackupService/PreviewRetention", req, g.inner.PreviewRetention)
}

// -----------------------------------------------------------------------------------------------
// Identity
// -----------------------------------------------------------------------------------------------

type identityGuard struct {
	// Required by the generated interface, and load-bearing: it is what answers a method this
	// decorator forgot to override, with Unimplemented rather than with the real service.
	fwv1.UnimplementedIdentityServiceServer

	e     *Enforcer
	inner fwv1.IdentityServiceServer
}

// GuardIdentity wraps the identity service.
func GuardIdentity(e *Enforcer, inner fwv1.IdentityServiceServer) fwv1.IdentityServiceServer {
	return &identityGuard{e: e, inner: inner}
}

var _ fwv1.IdentityServiceServer = (*identityGuard)(nil)

func (g *identityGuard) GetMe(ctx context.Context, req *fwv1.GetMeRequest) (*fwv1.GetMeResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.IdentityService/GetMe", req, g.inner.GetMe)
}

func (g *identityGuard) CreateToken(ctx context.Context, req *fwv1.CreateTokenRequest) (*fwv1.CreateTokenResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.IdentityService/CreateToken", req, g.inner.CreateToken)
}

func (g *identityGuard) ListTokens(ctx context.Context, req *fwv1.ListTokensRequest) (*fwv1.ListTokensResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.IdentityService/ListTokens", req, g.inner.ListTokens)
}

func (g *identityGuard) RevokeToken(ctx context.Context, req *fwv1.RevokeTokenRequest) (*fwv1.RevokeTokenResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.IdentityService/RevokeToken", req, g.inner.RevokeToken)
}

func (g *identityGuard) ListAuditLog(ctx context.Context, req *fwv1.ListAuditLogRequest) (*fwv1.ListAuditLogResponse, error) {
	return guarded(g.e, ctx, "/fleetward.v1.IdentityService/ListAuditLog", req, g.inner.ListAuditLog)
}
