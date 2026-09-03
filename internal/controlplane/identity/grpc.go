package identity

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// GRPCServer adapts the identity service to the generated IdentityService contract.
//
// Translation only, exactly like the other services: request in, domain call, response out, error
// mapped to a status code.
type GRPCServer struct {
	fwv1.UnimplementedIdentityServiceServer

	svc *Service
	log *slog.Logger
}

// NewGRPCServer wraps a service.
func NewGRPCServer(svc *Service, log *slog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log.With(slog.String("component", "identity-api"))}
}

var _ fwv1.IdentityServiceServer = (*GRPCServer)(nil)

// GetMe reports the caller.
func (g *GRPCServer) GetMe(ctx context.Context, _ *fwv1.GetMeRequest) (*fwv1.GetMeResponse, error) {
	resp, err := g.svc.Me(ctx)
	if err != nil {
		return nil, g.fail(ctx, "get me", err)
	}
	return resp, nil
}

// CreateToken mints a credential and returns its secret, once.
func (g *GRPCServer) CreateToken(ctx context.Context, req *fwv1.CreateTokenRequest) (*fwv1.CreateTokenResponse, error) {
	token, secret, err := g.svc.CreateToken(ctx, CreateTokenInput{
		Email:         req.GetEmail(),
		DisplayName:   req.GetDisplayName(),
		Role:          req.GetRole(),
		EnvironmentID: req.GetEnvironmentId(),
		InstanceID:    req.GetInstanceId(),
		Description:   req.GetDescription(),
		TTL:           req.GetTtl().AsDuration(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create token", err)
	}
	return &fwv1.CreateTokenResponse{Token: token, Secret: secret}, nil
}

// ListTokens reports the credentials issued in this tenant.
func (g *GRPCServer) ListTokens(ctx context.Context, req *fwv1.ListTokensRequest) (*fwv1.ListTokensResponse, error) {
	tokens, err := g.svc.ListTokens(ctx, req.GetIncludeInactive())
	if err != nil {
		return nil, g.fail(ctx, "list tokens", err)
	}
	return &fwv1.ListTokensResponse{Tokens: tokens}, nil
}

// RevokeToken stops a credential working.
func (g *GRPCServer) RevokeToken(ctx context.Context, req *fwv1.RevokeTokenRequest) (*fwv1.RevokeTokenResponse, error) {
	if err := g.svc.RevokeToken(ctx, req.GetTokenId()); err != nil {
		return nil, g.fail(ctx, "revoke token", err)
	}
	return &fwv1.RevokeTokenResponse{}, nil
}

// ListAuditLog reads the append-only record.
func (g *GRPCServer) ListAuditLog(ctx context.Context, req *fwv1.ListAuditLogRequest) (*fwv1.ListAuditLogResponse, error) {
	entries, next, err := g.svc.ListAuditLog(ctx, ListAuditInput{
		Actor:        req.GetActor(),
		Action:       req.GetAction(),
		ResourceType: req.GetResourceType(),
		ResourceID:   req.GetResourceId(),
		FailuresOnly: req.GetFailuresOnly(),
		PageSize:     req.GetPageSize(),
		PageToken:    req.GetPageToken(),
	})
	if err != nil {
		return nil, g.fail(ctx, "list audit log", err)
	}
	return &fwv1.ListAuditLogResponse{Entries: entries, NextPageToken: next}, nil
}

func (g *GRPCServer) fail(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request timed out")
	}

	// Never the error text. This service handles credentials, and an internal error's message is
	// the one place a fragment of one could reach a client by accident.
	g.log.ErrorContext(ctx, "identity request failed",
		slog.String("operation", operation),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}
