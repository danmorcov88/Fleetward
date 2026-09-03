package scheduler

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

// GRPCServer adapts the schedule service to the generated ScheduleService contract.
//
// It is translation only, for the same reason the other two are: anything with logic in it here is
// logic the CLI and the UI would each have to reimplement.
type GRPCServer struct {
	fwv1.UnimplementedScheduleServiceServer

	svc *Service
	log *slog.Logger
}

// NewGRPCServer wraps a service.
func NewGRPCServer(svc *Service, log *slog.Logger) *GRPCServer {
	return &GRPCServer{svc: svc, log: log.With(slog.String("component", "scheduler-api"))}
}

var _ fwv1.ScheduleServiceServer = (*GRPCServer)(nil)

// ListSchedules returns the tenant's schedules.
func (g *GRPCServer) ListSchedules(ctx context.Context, req *fwv1.ListSchedulesRequest) (*fwv1.ListSchedulesResponse, error) {
	schedules, err := g.svc.ListSchedules(ctx, req.GetInstanceId())
	if err != nil {
		return nil, g.fail(ctx, "list schedules", err)
	}
	return &fwv1.ListSchedulesResponse{Schedules: schedules}, nil
}

// GetSchedule returns one schedule.
func (g *GRPCServer) GetSchedule(ctx context.Context, req *fwv1.GetScheduleRequest) (*fwv1.GetScheduleResponse, error) {
	schedule, err := g.svc.GetSchedule(ctx, req.GetScheduleId())
	if err != nil {
		return nil, g.fail(ctx, "get schedule", err)
	}
	return &fwv1.GetScheduleResponse{Schedule: schedule}, nil
}

// CreateSchedule declares a recurring intent and computes its first run.
func (g *GRPCServer) CreateSchedule(ctx context.Context, req *fwv1.CreateScheduleRequest) (*fwv1.CreateScheduleResponse, error) {
	schedule, err := g.svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:           req.GetInstanceId(),
		Kind:                 jobKindName(req.GetKind()),
		CronExpression:       req.GetCronExpression(),
		Timezone:             req.GetTimezone(),
		MethodID:             req.GetMethodId(),
		Options:              req.GetOptions(),
		ExpectedCron:         req.GetExpectedCron(),
		ExpectedGraceMinutes: req.GetExpectedGraceMinutes(),
		VerifyPolicy:         verifyPolicyName(req.GetVerifyPolicy()),
		VerifySamplePct:      req.GetVerifySamplePercent(),
		RetentionDays:        req.GetRetentionDays(),
	})
	if err != nil {
		return nil, g.fail(ctx, "create schedule", err)
	}
	return &fwv1.CreateScheduleResponse{Schedule: schedule}, nil
}

// SetScheduleEnabled pauses or resumes a schedule.
func (g *GRPCServer) SetScheduleEnabled(ctx context.Context, req *fwv1.SetScheduleEnabledRequest) (*fwv1.SetScheduleEnabledResponse, error) {
	schedule, err := g.svc.SetScheduleEnabled(ctx, req.GetScheduleId(), req.GetEnabled())
	if err != nil {
		return nil, g.fail(ctx, "set schedule enabled", err)
	}
	return &fwv1.SetScheduleEnabledResponse{Schedule: schedule}, nil
}

// DeleteSchedule removes a schedule, leaving the jobs it created in place.
func (g *GRPCServer) DeleteSchedule(ctx context.Context, req *fwv1.DeleteScheduleRequest) (*fwv1.DeleteScheduleResponse, error) {
	if err := g.svc.DeleteSchedule(ctx, req.GetScheduleId()); err != nil {
		return nil, g.fail(ctx, "delete schedule", err)
	}
	return &fwv1.DeleteScheduleResponse{}, nil
}

// ListJobs reports what the scheduler actually did.
func (g *GRPCServer) ListJobs(ctx context.Context, req *fwv1.ListJobsRequest) (*fwv1.ListJobsResponse, error) {
	jobs, err := g.svc.ListJobs(ctx, ListJobsFilter{
		InstanceID: req.GetInstanceId(),
		ScheduleID: req.GetScheduleId(),
		State:      jobStateName(req.GetState()),
		PageSize:   req.GetPageSize(),
	})
	if err != nil {
		return nil, g.fail(ctx, "list jobs", err)
	}
	return &fwv1.ListJobsResponse{Jobs: jobs}, nil
}

// fail maps a service error to a status code. Only errors this service classified deliberately
// carry their message to the client; anything else is logged in full and returned as internal.
func (g *GRPCServer) fail(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, inventory.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, inventory.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request timed out")
	}

	g.log.ErrorContext(ctx, "schedule request failed",
		slog.String("operation", operation),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}

// -----------------------------------------------------------------------------------------------
// Enum mapping
//
// The database stores these as the lowercase strings its CHECK constraints spell, and the contract
// carries enums. The two vocabularies are translated in exactly one place so that a new value has
// one edit rather than several.
// -----------------------------------------------------------------------------------------------

func jobKindName(k fwv1.JobKind) string {
	switch k {
	case fwv1.JobKind_JOB_KIND_BACKUP:
		return kindBackup
	case fwv1.JobKind_JOB_KIND_VERIFY:
		return kindVerify
	case fwv1.JobKind_JOB_KIND_RESTORE:
		return "restore"
	case fwv1.JobKind_JOB_KIND_DISCOVERY:
		return "discovery"
	case fwv1.JobKind_JOB_KIND_METRICS:
		return "metrics"
	case fwv1.JobKind_JOB_KIND_OBSERVE:
		return kindObserve
	case fwv1.JobKind_JOB_KIND_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func jobKindFromName(name string) fwv1.JobKind {
	switch name {
	case kindBackup:
		return fwv1.JobKind_JOB_KIND_BACKUP
	case kindVerify:
		return fwv1.JobKind_JOB_KIND_VERIFY
	case "restore":
		return fwv1.JobKind_JOB_KIND_RESTORE
	case "discovery":
		return fwv1.JobKind_JOB_KIND_DISCOVERY
	case "metrics":
		return fwv1.JobKind_JOB_KIND_METRICS
	case kindObserve:
		return fwv1.JobKind_JOB_KIND_OBSERVE
	default:
		return fwv1.JobKind_JOB_KIND_UNSPECIFIED
	}
}

func jobStateName(s fwv1.JobState) string {
	switch s {
	case fwv1.JobState_JOB_STATE_PENDING:
		return "pending"
	case fwv1.JobState_JOB_STATE_RUNNING:
		return "running"
	case fwv1.JobState_JOB_STATE_SUCCEEDED:
		return "succeeded"
	case fwv1.JobState_JOB_STATE_FAILED:
		return "failed"
	case fwv1.JobState_JOB_STATE_CANCELED:
		return "canceled"
	case fwv1.JobState_JOB_STATE_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func jobStateFromName(name string) fwv1.JobState {
	switch name {
	case "pending":
		return fwv1.JobState_JOB_STATE_PENDING
	case "running":
		return fwv1.JobState_JOB_STATE_RUNNING
	case "succeeded":
		return fwv1.JobState_JOB_STATE_SUCCEEDED
	case "failed":
		return fwv1.JobState_JOB_STATE_FAILED
	case "canceled":
		return fwv1.JobState_JOB_STATE_CANCELED
	default:
		return fwv1.JobState_JOB_STATE_UNSPECIFIED
	}
}

func verifyPolicyName(p fwv1.VerifyPolicy) string {
	switch p {
	case fwv1.VerifyPolicy_VERIFY_POLICY_ALWAYS:
		return verifyAlways
	case fwv1.VerifyPolicy_VERIFY_POLICY_SAMPLED:
		return verifySampled
	case fwv1.VerifyPolicy_VERIFY_POLICY_MANUAL:
		return verifyManual
	case fwv1.VerifyPolicy_VERIFY_POLICY_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func verifyPolicyFromName(name string) fwv1.VerifyPolicy {
	switch name {
	case verifyAlways:
		return fwv1.VerifyPolicy_VERIFY_POLICY_ALWAYS
	case verifySampled:
		return fwv1.VerifyPolicy_VERIFY_POLICY_SAMPLED
	case verifyManual:
		return fwv1.VerifyPolicy_VERIFY_POLICY_MANUAL
	default:
		return fwv1.VerifyPolicy_VERIFY_POLICY_UNSPECIFIED
	}
}
