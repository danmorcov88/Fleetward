package api

import (
	"context"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// NewGatewayMux builds the grpc-gateway router that serves the REST API (ADR-0004).
//
// Services are registered onto it with their generated RegisterXHandlerServer function, which wires
// the handler straight to the service implementation in-process. There is no loopback gRPC dial and
// therefore no second listener to secure: the only network surface is the HTTP server this mux is
// mounted on.
func NewGatewayMux() *runtime.ServeMux {
	return runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				// The contract is the documentation, so the JSON uses the field names that appear in
				// the .proto rather than protojson's lowerCamelCase rewriting of them.
				UseProtoNames: true,
				// Without this an empty list is omitted entirely, so `GET /instances` on a new
				// installation returns `{}` and every client has to special-case it.
				EmitDefaultValues: true,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				// Unknown fields are rejected on purpose. Silently dropping a field whose name was
				// mistyped turns a one-character mistake into the far more confusing
				// "environment_id is required" for a request that plainly supplied it.
				DiscardUnknown: false,
			},
		}),
		runtime.WithErrorHandler(problemErrorHandler),
	)
}

// problemErrorHandler renders a gateway error in the single problem-details shape the rest of the
// API uses, so a client never has to handle two error formats depending on which handler failed.
func problemErrorHandler(
	ctx context.Context,
	_ *runtime.ServeMux,
	_ runtime.Marshaler,
	w http.ResponseWriter,
	_ *http.Request,
	err error,
) {
	st := status.Convert(err)
	httpStatus := runtime.HTTPStatusFromCode(st.Code())
	WriteProblem(w, httpStatus, http.StatusText(httpStatus), st.Message(), telemetry.RequestIDFrom(ctx))
}
