package sdk

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/version"
)

// PluginName is the key under which the engine implementation is registered with go-plugin.
const PluginName = "engine"

// ProtocolVersion is bumped only when the go-plugin transport itself changes shape. Additive
// changes to the EnginePlugin service do not touch it — `buf breaking` guards those (ADR-0004).
const ProtocolVersion = 1

// Handshake is the mutual identification between core and a plugin. The magic cookie is not a
// security control; it exists so that running a plugin binary by hand prints a helpful message
// instead of hanging on a handshake that will never complete.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "FLEETWARD_PLUGIN",
	MagicCookieValue: "fleetward-engine-plugin-v1",
}

// PluginMap is the go-plugin registration shared by core and plugins.
func PluginMap(engine Engine) map[string]goplugin.Plugin {
	return map[string]goplugin.Plugin{
		PluginName: &EnginePlugin{Impl: engine},
	}
}

// EnginePlugin adapts an Engine to go-plugin's gRPC plugin interface.
type EnginePlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	// Impl is set on the plugin side and is nil on the host side.
	Impl Engine
}

var _ goplugin.GRPCPlugin = (*EnginePlugin)(nil)

// GRPCServer registers the engine implementation. Called in the plugin process.
func (p *EnginePlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	fwv1.RegisterEnginePluginServer(s, &grpcServer{impl: p.Impl})
	return nil
}

// GRPCClient returns the generated client. Called in the host process.
//
// Returning the generated client directly, rather than wrapping it, keeps exactly one definition
// of the contract in play on both sides of the boundary.
func (p *EnginePlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return fwv1.NewEnginePluginClient(c), nil
}

// Serve runs the plugin. It blocks until the host disconnects, then returns; a plugin's main
// function should do nothing after calling it.
func Serve(engine Engine) {
	if engine == nil {
		fmt.Fprintln(os.Stderr, "fleetward plugin: Serve called with a nil engine")
		os.Exit(1)
	}

	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         PluginMap(engine),
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts,
				grpc.ChainUnaryInterceptor(recoverUnary),
				grpc.ChainStreamInterceptor(recoverStream),
			)
			return grpc.NewServer(opts...)
		},
		// Plugin logs are forwarded to the host, which re-emits them through its own slog handler,
		// so a plugin's diagnostics end up in the same stream as everything else.
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:       "plugin",
			Level:      hclog.Info,
			Output:     os.Stderr,
			JSONFormat: true,
		}),
	})
}

// ServeWithLogger is Serve with a caller-supplied log level, used by plugin binaries that accept a
// --log-level flag.
func ServeWithLogger(engine Engine, level slog.Level) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         PluginMap(engine),
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts,
				grpc.ChainUnaryInterceptor(recoverUnary),
				grpc.ChainStreamInterceptor(recoverStream),
			)
			return grpc.NewServer(opts...)
		},
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:       "plugin",
			Level:      hclogLevel(level),
			Output:     os.Stderr,
			JSONFormat: true,
		}),
	})
}

func hclogLevel(level slog.Level) hclog.Level {
	switch {
	case level <= slog.LevelDebug:
		return hclog.Debug
	case level <= slog.LevelInfo:
		return hclog.Info
	case level <= slog.LevelWarn:
		return hclog.Warn
	default:
		return hclog.Error
	}
}

// recoverUnary turns a panic into an error.
//
// The manager would restart a crashed plugin anyway, but a mid-backup crash loses the run and the
// diagnostic context with it. Converting the panic keeps the stack trace attached to the failure
// the operator actually sees.
func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = toStatus(Internal("panic in %s: %v\n%s", info.FullMethod, r, debug.Stack()))
			resp = nil
		}
	}()
	resp, err = handler(ctx, req)
	return resp, toStatus(err)
}

func recoverStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = toStatus(Internal("panic in %s: %v\n%s", info.FullMethod, r, debug.Stack()))
		}
	}()
	return toStatus(handler(srv, ss))
}

// grpcServer adapts the Engine interface to the generated gRPC service.
type grpcServer struct {
	fwv1.UnimplementedEnginePluginServer
	impl Engine
}

func (s *grpcServer) GetCapabilities(ctx context.Context, _ *fwv1.GetCapabilitiesRequest) (*fwv1.Capabilities, error) {
	caps, err := s.impl.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	// Stamping the contract version here rather than trusting each plugin to remember means the
	// manager's compatibility check cannot be defeated by an author who simply left the field blank.
	if caps.GetContractVersion() == "" {
		caps.ContractVersion = version.ContractVersion
	}
	if err := ValidateCapabilities(caps); err != nil {
		return nil, err
	}
	return caps, nil
}

func (s *grpcServer) Discover(ctx context.Context, req *fwv1.DiscoverRequest) (*fwv1.DiscoverResponse, error) {
	return s.impl.Discover(ctx, req)
}

func (s *grpcServer) GetConfig(ctx context.Context, req *fwv1.GetConfigRequest) (*fwv1.GetConfigResponse, error) {
	return s.impl.GetConfig(ctx, req)
}

func (s *grpcServer) CollectMetrics(req *fwv1.CollectMetricsRequest, stream fwv1.EnginePlugin_CollectMetricsServer) error {
	return s.impl.CollectMetrics(stream.Context(), req, stream.Send)
}

func (s *grpcServer) Backup(req *fwv1.BackupRequest, stream fwv1.EnginePlugin_BackupServer) error {
	return s.impl.Backup(stream.Context(), req, stream.Send)
}

func (s *grpcServer) Restore(req *fwv1.RestoreRequest, stream fwv1.EnginePlugin_RestoreServer) error {
	return s.impl.Restore(stream.Context(), req, stream.Send)
}

func (s *grpcServer) VerifyRestore(ctx context.Context, req *fwv1.VerifyRestoreRequest) (*fwv1.VerifyRestoreResult, error) {
	return s.impl.VerifyRestore(ctx, req)
}

func (s *grpcServer) ListPITRTargets(ctx context.Context, req *fwv1.ListPITRTargetsRequest) (*fwv1.PITRWindow, error) {
	return s.impl.ListPITRTargets(ctx, req)
}

func (s *grpcServer) ListPrincipals(ctx context.Context, req *fwv1.ListPrincipalsRequest) (*fwv1.ListPrincipalsResponse, error) {
	return s.impl.ListPrincipals(ctx, req)
}

func (s *grpcServer) HealthCheck(ctx context.Context, req *fwv1.HealthCheckRequest) (*fwv1.HealthStatus, error) {
	return s.impl.HealthCheck(ctx, req)
}
