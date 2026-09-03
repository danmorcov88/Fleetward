package backup

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// GRPCServer adapts the backup service to the generated BackupService contract.
//
// It is translation only. Anything with logic in it here is logic the CLI, the UI, and the
// scheduler would each have to reimplement.
type GRPCServer struct {
	fwv1.UnimplementedBackupServiceServer

	svc *Service
	log *slog.Logger
}

// NewGRPCServer wraps a service.
func NewGRPCServer(svc *Service, log *slog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log.With(slog.String("component", "backup-api"))}
}

var _ fwv1.BackupServiceServer = (*GRPCServer)(nil)

// RunBackup starts a backup and returns its identifiers. It does not wait for the backup to finish;
// the caller polls GetBackup.
func (g *GRPCServer) RunBackup(ctx context.Context, req *fwv1.RunBackupRequest) (*fwv1.RunBackupResponse, error) {
	backupID, jobID, err := g.svc.RunBackup(ctx, RunBackupInput{
		InstanceID: req.GetInstanceId(),
		MethodID:   req.GetMethodId(),
		Options:    req.GetOptions(),
		Databases:  req.GetDatabases(),
		// A per-request override of the instance's policy. The policy itself — always, sampled, or
		// manual — belongs to the scheduler in phase B; what this flag does is chain one
		// verification onto this one backup, which is the product's core loop written out by hand.
		VerifyOnCompletion: req.GetVerifyOnCompletion(),
		// Everything reaching this RPC today was asked for by a human; the scheduler in phase B is
		// what will set this to false.
		TriggeredManually: true,
	})
	if err != nil {
		return nil, g.fail(ctx, "run backup", err)
	}
	return &fwv1.RunBackupResponse{BackupId: backupID, JobId: jobID}, nil
}

// GetBackup returns one backup and the manifest captured when it was taken.
func (g *GRPCServer) GetBackup(ctx context.Context, req *fwv1.GetBackupRequest) (*fwv1.GetBackupResponse, error) {
	b, manifest, err := g.svc.GetBackup(ctx, req.GetBackupId())
	if err != nil {
		return nil, g.fail(ctx, "get backup", err)
	}
	return &fwv1.GetBackupResponse{Backup: b, Manifest: manifest}, nil
}

// ListBackups reports backup history across both origins.
func (g *GRPCServer) ListBackups(ctx context.Context, req *fwv1.ListBackupsRequest) (*fwv1.ListBackupsResponse, error) {
	backups, err := g.svc.ListBackups(ctx, ListBackupsInput{
		InstanceID:    req.GetInstanceId(),
		EnvironmentID: req.GetEnvironmentId(),
		State:         req.GetState(),
		Origin:        req.GetOrigin(),
		PageSize:      req.GetPageSize(),
	})
	if err != nil {
		return nil, g.fail(ctx, "list backups", err)
	}
	// The service clamps a page to maxListPageSize, so this is a small number by construction.
	return &fwv1.ListBackupsResponse{
		Backups:   backups,
		TotalSize: int32(len(backups)), //nolint:gosec // G115: bounded by maxListPageSize
	}, nil
}

// ObserveBackupHistory reads the engine's own record of backups Fleetward did not take.
func (g *GRPCServer) ObserveBackupHistory(ctx context.Context, req *fwv1.ObserveBackupHistoryRequest) (*fwv1.ObserveBackupHistoryResponse, error) {
	result, err := g.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: req.GetInstanceId()})
	if err != nil {
		return nil, g.fail(ctx, "observe backup history", err)
	}
	out := &fwv1.ObserveBackupHistoryResponse{
		Discovered: result.Discovered,
		Updated:    result.Updated,
	}
	if !result.Watermark.IsZero() {
		out.Watermark = timestamppb.New(result.Watermark)
	}
	return out, nil
}

// GetBackupAdherence reports the gap between what was declared and what was detected.
func (g *GRPCServer) GetBackupAdherence(ctx context.Context, req *fwv1.GetBackupAdherenceRequest) (*fwv1.GetBackupAdherenceResponse, error) {
	instances, err := g.svc.GetBackupAdherence(ctx, GetAdherenceInput{
		InstanceID:    req.GetInstanceId(),
		EnvironmentID: req.GetEnvironmentId(),
		ProblemsOnly:  req.GetProblemsOnly(),
	})
	if err != nil {
		return nil, g.fail(ctx, "get backup adherence", err)
	}
	return &fwv1.GetBackupAdherenceResponse{Instances: instances}, nil
}

// RunVerification restores a backup into a sandbox and smoke-tests it, returning its identifiers.
// It does not wait for the verification to finish; the caller polls GetVerification.
func (g *GRPCServer) RunVerification(ctx context.Context, req *fwv1.RunVerificationRequest) (*fwv1.RunVerificationResponse, error) {
	verificationID, jobID, err := g.svc.RunVerification(ctx, RunVerificationInput{
		BackupID: req.GetBackupId(),
		Checks:   req.GetChecks(),
	})
	if err != nil {
		return nil, g.fail(ctx, "run verification", err)
	}
	return &fwv1.RunVerificationResponse{VerificationId: verificationID, JobId: jobID}, nil
}

// GetVerification returns one verification and every check it ran.
func (g *GRPCServer) GetVerification(ctx context.Context, req *fwv1.GetVerificationRequest) (*fwv1.GetVerificationResponse, error) {
	verification, err := g.svc.GetVerification(ctx, req.GetVerificationId())
	if err != nil {
		return nil, g.fail(ctx, "get verification", err)
	}
	return &fwv1.GetVerificationResponse{Verification: verification}, nil
}

// PreviewRetention reports what the next retention sweep would delete, and what it would not.
//
// Read-only, and deliberately available whether or not the sweep is enabled: an operator deciding
// whether to turn retention on is exactly the person who needs to see the answer.
func (g *GRPCServer) PreviewRetention(ctx context.Context, req *fwv1.PreviewRetentionRequest) (*fwv1.PreviewRetentionResponse, error) {
	out, err := g.svc.PreviewRetention(ctx, PreviewRetentionInput{InstanceID: req.GetInstanceId()})
	if err != nil {
		return nil, g.fail(ctx, "preview retention", err)
	}
	return out, nil
}

// GetPITRWindow is phase B, and depends on WAL archiving that no method produces yet.
func (g *GRPCServer) GetPITRWindow(context.Context, *fwv1.GetPITRWindowRequest) (*fwv1.GetPITRWindowResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"point-in-time recovery is not implemented yet; it arrives in phase B")
}

// fail maps a service error to a status code.
//
// Only errors this service classified deliberately carry their message to the client. Anything else
// is logged in full and returned as a bare internal error: an unexpected failure from pgx, from the
// secrets provider, or from the object store can contain a connection string or a presigned URL,
// and a client is exactly where those must not appear.
func (g *GRPCServer) fail(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, inventory.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, inventory.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrAlreadyRunning):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, ErrNotVerifiable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrEngineUnavailable), errors.Is(err, inventory.ErrEngineUnavailable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrPluginFailed), errors.Is(err, inventory.ErrPluginFailed):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request timed out")
	}

	g.log.ErrorContext(ctx, "backup request failed",
		slog.String("operation", operation),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}
