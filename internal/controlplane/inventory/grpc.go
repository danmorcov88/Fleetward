package inventory

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// GRPCServer adapts the inventory service to the generated InventoryService contract.
//
// It is translation only: request in, domain call, response out, error mapped to a status code. Any
// logic that appears here is logic the CLI, the UI, and the scheduler would each have to
// reimplement, which is the reason for keeping the layer this thin.
type GRPCServer struct {
	fwv1.UnimplementedInventoryServiceServer

	svc *Service
	log *slog.Logger
}

// NewGRPCServer wraps a service.
func NewGRPCServer(svc *Service, log *slog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log.With(slog.String("component", "inventory-api"))}
}

var _ fwv1.InventoryServiceServer = (*GRPCServer)(nil)

// ListEnvironments returns the tenant's environments.
func (g *GRPCServer) ListEnvironments(ctx context.Context, req *fwv1.ListEnvironmentsRequest) (*fwv1.ListEnvironmentsResponse, error) {
	envs, page, err := g.svc.ListEnvironments(ctx, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, g.fail(ctx, "list environments", err)
	}
	return &fwv1.ListEnvironmentsResponse{Environments: envs, NextPageToken: page.NextPageToken}, nil
}

// CreateEnvironment adds an environment.
func (g *GRPCServer) CreateEnvironment(ctx context.Context, req *fwv1.CreateEnvironmentRequest) (*fwv1.CreateEnvironmentResponse, error) {
	env, err := g.svc.CreateEnvironment(ctx, CreateEnvironmentInput{
		Name:         req.GetName(),
		Description:  req.GetDescription(),
		IsProduction: req.GetIsProduction(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create environment", err)
	}
	return &fwv1.CreateEnvironmentResponse{Environment: env}, nil
}

// ListInstances returns the tenant's instances.
func (g *GRPCServer) ListInstances(ctx context.Context, req *fwv1.ListInstancesRequest) (*fwv1.ListInstancesResponse, error) {
	instances, page, err := g.svc.ListInstances(ctx, ListInstancesFilter{
		EnvironmentID: req.GetEnvironmentId(),
		EngineType:    req.GetEngineType(),
		PageSize:      req.GetPageSize(),
		PageToken:     req.GetPageToken(),
	})
	if err != nil {
		return nil, g.fail(ctx, "list instances", err)
	}
	return &fwv1.ListInstancesResponse{
		Instances:     instances,
		NextPageToken: page.NextPageToken,
		TotalSize:     page.TotalSize,
	}, nil
}

// GetInstance returns one instance with the capabilities of the plugin serving it.
func (g *GRPCServer) GetInstance(ctx context.Context, req *fwv1.GetInstanceRequest) (*fwv1.GetInstanceResponse, error) {
	resp, err := g.svc.GetInstance(ctx, req.GetInstanceId())
	if err != nil {
		return nil, g.fail(ctx, "get instance", err)
	}
	return resp, nil
}

// CreateInstance stores an instance and its credentials.
func (g *GRPCServer) CreateInstance(ctx context.Context, req *fwv1.CreateInstanceRequest) (*fwv1.CreateInstanceResponse, error) {
	inst, err := g.svc.CreateInstance(ctx, CreateInstanceInput{
		EnvironmentID: req.GetEnvironmentId(),
		Name:          req.GetName(),
		EngineType:    req.GetEngineType(),
		Host:          req.GetHost(),
		Port:          req.GetPort(),
		Labels:        req.GetLabels(),
		Connection:    req.GetConnection(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create instance", err)
	}
	return &fwv1.CreateInstanceResponse{Instance: inst}, nil
}

// DeleteInstance removes an instance and its credentials.
func (g *GRPCServer) DeleteInstance(ctx context.Context, req *fwv1.DeleteInstanceRequest) (*fwv1.DeleteInstanceResponse, error) {
	if err := g.svc.DeleteInstance(ctx, req.GetInstanceId(), req.GetDeleteArtifacts()); err != nil {
		return nil, g.fail(ctx, "delete instance", err)
	}
	return &fwv1.DeleteInstanceResponse{}, nil
}

// TestConnection health-checks an instance, stored or proposed.
func (g *GRPCServer) TestConnection(ctx context.Context, req *fwv1.TestConnectionRequest) (*fwv1.TestConnectionResponse, error) {
	resp, err := g.svc.TestConnection(ctx, req)
	if err != nil {
		return nil, g.fail(ctx, "test connection", err)
	}
	return resp, nil
}

// DiscoverInstance refreshes topology, version, and databases.
func (g *GRPCServer) DiscoverInstance(ctx context.Context, req *fwv1.DiscoverInstanceRequest) (*fwv1.DiscoverInstanceResponse, error) {
	resp, err := g.svc.DiscoverInstance(ctx, req.GetInstanceId())
	if err != nil {
		return nil, g.fail(ctx, "discover instance", err)
	}
	return resp, nil
}

// ListPrincipalsForInstance is Phase C. It is declared unimplemented rather than left to the
// generated stub so the message says which slice will deliver it.
func (g *GRPCServer) ListPrincipalsForInstance(context.Context, *fwv1.ListPrincipalsForInstanceRequest) (*fwv1.ListPrincipalsForInstanceResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"access compliance is not implemented yet; it arrives in phase C")
}

// fail maps a service error to a status code.
//
// Only errors this service classified deliberately carry their message to the client. Anything else
// is logged in full and returned as a bare internal error: an unexpected failure from pgx or from
// the secrets provider can contain a connection string, and a client is exactly where it must not
// appear.
func (g *GRPCServer) fail(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrEngineUnavailable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrPluginFailed):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request timed out")
	}

	g.log.ErrorContext(ctx, "inventory request failed",
		slog.String("operation", operation),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}
