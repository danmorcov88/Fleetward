package backup

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	if req.GetVerifyOnCompletion() {
		// Accepting the flag and ignoring it would promise a verification that never happens, which
		// is precisely the failure mode this product exists to prevent.
		return nil, status.Error(codes.Unimplemented,
			"verify_on_completion is not implemented yet; automated verification arrives in slice A5")
	}

	backupID, jobID, err := g.svc.RunBackup(ctx, RunBackupInput{
		InstanceID: req.GetInstanceId(),
		MethodID:   req.GetMethodId(),
		Options:    req.GetOptions(),
		Databases:  req.GetDatabases(),
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

// ListBackups is phase B. Backup history has to account for observed backups and their origin
// (ADR-0015), and a listing built now without that column would have to be rebuilt then.
func (g *GRPCServer) ListBackups(context.Context, *fwv1.ListBackupsRequest) (*fwv1.ListBackupsResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"backup history is not implemented yet; it arrives in phase B with observed backups")
}

// RunVerification is slice A5.
func (g *GRPCServer) RunVerification(context.Context, *fwv1.RunVerificationRequest) (*fwv1.RunVerificationResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"backup verification is not implemented yet; it arrives in slice A5")
}

// GetVerification is slice A5.
func (g *GRPCServer) GetVerification(context.Context, *fwv1.GetVerificationRequest) (*fwv1.GetVerificationResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"backup verification is not implemented yet; it arrives in slice A5")
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
