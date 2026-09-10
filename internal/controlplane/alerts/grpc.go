package alerts

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// GRPCServer adapts the alerts service to the generated AlertService contract.
//
// Translation only, exactly like the other services: request in, domain call, response out, error
// mapped to a status code.
type GRPCServer struct {
	fwv1.UnimplementedAlertServiceServer

	svc *Service
	log *slog.Logger
}

// NewGRPCServer wraps a service.
func NewGRPCServer(svc *Service, log *slog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log.With(slog.String("component", "alerts-api"))}
}

var _ fwv1.AlertServiceServer = (*GRPCServer)(nil)

// ListAlerts reports what is wrong, filtered to what the caller may see.
func (g *GRPCServer) ListAlerts(ctx context.Context, req *fwv1.ListAlertsRequest) (*fwv1.ListAlertsResponse, error) {
	alerts, err := g.svc.ListAlerts(ctx, ListAlertsInput{
		InstanceID:      req.GetInstanceId(),
		EnvironmentID:   req.GetEnvironmentId(),
		IncludeResolved: req.GetIncludeResolved(),
		MinSeverity:     req.GetMinSeverity(),
		PageSize:        req.GetPageSize(),
	})
	if err != nil {
		return nil, g.fail(ctx, "list alerts", err)
	}
	return &fwv1.ListAlertsResponse{Alerts: alerts}, nil
}

// AcknowledgeAlert records that somebody knows.
func (g *GRPCServer) AcknowledgeAlert(ctx context.Context, req *fwv1.AcknowledgeAlertRequest) (*fwv1.AcknowledgeAlertResponse, error) {
	alert, err := g.svc.AcknowledgeAlert(ctx, req.GetAlertId())
	if err != nil {
		return nil, g.fail(ctx, "acknowledge alert", err)
	}
	return &fwv1.AcknowledgeAlertResponse{Alert: alert}, nil
}

// ListAlertRules reports what this tenant is watching for.
func (g *GRPCServer) ListAlertRules(ctx context.Context, req *fwv1.ListAlertRulesRequest) (*fwv1.ListAlertRulesResponse, error) {
	rules, err := g.svc.ListAlertRules(ctx, req.GetIncludeDisabled())
	if err != nil {
		return nil, g.fail(ctx, "list alert rules", err)
	}
	return &fwv1.ListAlertRulesResponse{Rules: rules}, nil
}

// CreateAlertRule declares a condition worth being told about.
func (g *GRPCServer) CreateAlertRule(ctx context.Context, req *fwv1.CreateAlertRuleRequest) (*fwv1.CreateAlertRuleResponse, error) {
	rule, err := g.svc.CreateAlertRule(ctx, CreateRuleInput{
		Name:          req.GetName(),
		Description:   req.GetDescription(),
		Kind:          req.GetKind(),
		Severity:      req.GetSeverity(),
		Threshold:     req.GetThreshold(),
		EnvironmentID: req.GetEnvironmentId(),
		InstanceID:    req.GetInstanceId(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create alert rule", err)
	}
	return &fwv1.CreateAlertRuleResponse{Rule: rule}, nil
}

// SetAlertRuleEnabled silences or resumes a rule.
func (g *GRPCServer) SetAlertRuleEnabled(ctx context.Context, req *fwv1.SetAlertRuleEnabledRequest) (*fwv1.SetAlertRuleEnabledResponse, error) {
	rule, err := g.svc.SetAlertRuleEnabled(ctx, req.GetRuleId(), req.GetEnabled())
	if err != nil {
		return nil, g.fail(ctx, "set alert rule enabled", err)
	}
	return &fwv1.SetAlertRuleEnabledResponse{Rule: rule}, nil
}

// DeleteAlertRule removes a rule and resolves what it opened.
func (g *GRPCServer) DeleteAlertRule(ctx context.Context, req *fwv1.DeleteAlertRuleRequest) (*fwv1.DeleteAlertRuleResponse, error) {
	resolved, err := g.svc.DeleteAlertRule(ctx, req.GetRuleId())
	if err != nil {
		return nil, g.fail(ctx, "delete alert rule", err)
	}
	return &fwv1.DeleteAlertRuleResponse{AlertsResolved: resolved}, nil
}

// ListNotifiers reports where alerts are sent, and how that has been going.
func (g *GRPCServer) ListNotifiers(ctx context.Context, req *fwv1.ListNotifiersRequest) (*fwv1.ListNotifiersResponse, error) {
	notifiers, err := g.svc.ListNotifiers(ctx, req.GetIncludeDisabled())
	if err != nil {
		return nil, g.fail(ctx, "list notifiers", err)
	}
	return &fwv1.ListNotifiersResponse{Notifiers: notifiers}, nil
}

// CreateNotifier stores a destination and its credential, separately.
func (g *GRPCServer) CreateNotifier(ctx context.Context, req *fwv1.CreateNotifierRequest) (*fwv1.CreateNotifierResponse, error) {
	notifier, err := g.svc.CreateNotifier(ctx, CreateNotifierInput{
		Name:        req.GetName(),
		Kind:        req.GetKind(),
		Settings:    req.GetSettings(),
		Secret:      req.GetSecret(),
		MinSeverity: req.GetMinSeverity(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create notifier", err)
	}
	return &fwv1.CreateNotifierResponse{Notifier: notifier}, nil
}

// DeleteNotifier removes a destination and the credential it held.
func (g *GRPCServer) DeleteNotifier(ctx context.Context, req *fwv1.DeleteNotifierRequest) (*fwv1.DeleteNotifierResponse, error) {
	if err := g.svc.DeleteNotifier(ctx, req.GetNotifierId()); err != nil {
		return nil, g.fail(ctx, "delete notifier", err)
	}
	return &fwv1.DeleteNotifierResponse{}, nil
}

// TestNotifier sends a real message to a real endpoint.
//
// A delivery that failed is a successful RPC reporting `delivered: false`, not an error. The
// question asked was "does this work", and "no, because the endpoint answered 502" is an answer to
// it rather than a failure to answer.
func (g *GRPCServer) TestNotifier(ctx context.Context, req *fwv1.TestNotifierRequest) (*fwv1.TestNotifierResponse, error) {
	delivered, message, took, err := g.svc.TestNotifier(ctx, req.GetNotifierId())
	if err != nil {
		return nil, g.fail(ctx, "test notifier", err)
	}
	return &fwv1.TestNotifierResponse{
		Delivered: delivered,
		Error:     message,
		Duration:  durationpb.New(took),
	}, nil
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

	// Never the error text. This service stores a credential for somebody else's system, and an
	// internal error's message is the one place a fragment of one could reach a client by accident.
	g.log.ErrorContext(ctx, "alerts request failed",
		slog.String("operation", operation),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}
