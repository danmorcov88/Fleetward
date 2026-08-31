package sdk

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Error is a plugin failure carrying a structured PluginError.
//
// Plugins should return these rather than bare errors so that core can classify failures — is this
// retryable? is the tool missing? were the credentials wrong? — without parsing error strings,
// which is exactly the kind of coupling a plugin contract exists to prevent.
type Error struct {
	Code      fwv1.ErrorCode
	Message   string
	Details   map[string]string
	Retryable bool
	// Cause is wrapped for errors.Is and errors.As but is never sent over the wire: an underlying
	// driver error can contain a connection string.
	Cause error
}

// NewError builds a plugin error.
func NewError(code fwv1.ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap supports errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// WithCause attaches an underlying error for local logging and error inspection.
func (e *Error) WithCause(err error) *Error {
	e.Cause = err
	return e
}

// WithDetail adds a structured detail. Callers must not put credentials here — details cross the
// process boundary and end up in core's logs.
func (e *Error) WithDetail(key, value string) *Error {
	if e.Details == nil {
		e.Details = make(map[string]string)
	}
	e.Details[key] = value
	return e
}

// Retry marks the error as worth retrying.
func (e *Error) Retry() *Error {
	e.Retryable = true
	return e
}

// Proto converts the error to its wire representation. Only the message and details cross the
// boundary; the wrapped cause stays local.
func (e *Error) Proto() *fwv1.PluginError {
	return &fwv1.PluginError{
		Code:      e.Code,
		Message:   e.Message,
		Details:   e.Details,
		Retryable: e.Retryable,
	}
}

// Convenience constructors for the codes plugins reach for most.

// Unsupported reports that the engine does not implement a capability.
func Unsupported(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED, format, args...)
}

// ConnectionFailed reports that the instance could not be reached. It is retryable: a database
// that is briefly unreachable is the most common transient failure there is.
func ConnectionFailed(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED, format, args...).Retry()
}

// AuthenticationFailed reports rejected credentials. Deliberately not retryable: the same wrong
// password will stay wrong, and repeated attempts can trigger account lockout on the monitored
// instance.
func AuthenticationFailed(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_AUTHENTICATION_FAILED, format, args...)
}

// PermissionDenied reports that the connection's principal lacks a required privilege.
func PermissionDenied(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, format, args...)
}

// InvalidArgument reports a malformed request.
func InvalidArgument(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, format, args...)
}

// ToolNotFound reports a missing native executable, e.g. pg_basebackup.
func ToolNotFound(tool string) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_TOOL_NOT_FOUND,
		"required tool %q was not found on PATH", tool).WithDetail("tool", tool)
}

// ToolFailed reports that a native tool ran and exited non-zero.
func ToolFailed(tool string, format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_TOOL_FAILED, format, args...).WithDetail("tool", tool)
}

// DetailArtifact is the PluginError detail key a plugin sets when it can say something definitive
// about the artifact itself, rather than about the machinery around it.
//
// The distinction matters more here than anywhere else in the contract. "The artifact is not the
// bytes we wrote" is data loss and must reach an operator as a failed verification; "the download
// timed out" is an infrastructure problem and must not, because an alert that fires on flaky
// networks is an alert nobody reads. The two would otherwise be indistinguishable, since both are
// discovered while fetching an object.
const DetailArtifact = "artifact"

// ArtifactStateCorrupt is the DetailArtifact value meaning the artifact does not match what was
// recorded when it was written.
const ArtifactStateCorrupt = "corrupt"

// ArtifactCorrupt reports that an artifact failed the integrity check made against the checksum
// stored with it. It is deliberately not retryable: the same bytes will fail the same way.
func ArtifactCorrupt(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, format, args...).
		WithDetail(DetailArtifact, ArtifactStateCorrupt)
}

// IsArtifactCorrupt reports whether a plugin blamed the artifact itself.
func IsArtifactCorrupt(pe *fwv1.PluginError) bool {
	return pe.GetDetails()[DetailArtifact] == ArtifactStateCorrupt
}

// ObjectStoreFailed reports a failure transferring an artifact.
func ObjectStoreFailed(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_OBJECT_STORE_FAILED, format, args...).Retry()
}

// Internal reports an unexpected failure inside the plugin.
func Internal(format string, args ...any) *Error {
	return NewError(fwv1.ErrorCode_ERROR_CODE_INTERNAL, format, args...)
}

// SafeURL strips the query string from a presigned URL, leaving something safe to log.
//
// A presigned URL's signature is a bearer credential for the object: anyone holding the full URL
// can read or overwrite an artifact until it expires. It must never reach a log line or an error
// message, and both core and every plugin handle these URLs, which is why the redaction lives in
// the harness they share.
func SafeURL(rawURL string) string {
	for i := range len(rawURL) {
		if rawURL[i] == '?' {
			return rawURL[:i] + "?[signature redacted]"
		}
	}
	return rawURL
}

// AsPluginError extracts a wire PluginError from any error, classifying plain errors as best it
// can so that core always receives structured information.
func AsPluginError(err error) *fwv1.PluginError {
	if err == nil {
		return nil
	}

	var pluginErr *Error
	if errors.As(err, &pluginErr) {
		return pluginErr.Proto()
	}

	switch {
	case errors.Is(err, context.Canceled):
		return &fwv1.PluginError{
			Code:    fwv1.ErrorCode_ERROR_CODE_CANCELED,
			Message: "operation canceled",
		}
	case errors.Is(err, context.DeadlineExceeded):
		return &fwv1.PluginError{
			Code:      fwv1.ErrorCode_ERROR_CODE_TIMEOUT,
			Message:   "operation timed out",
			Retryable: true,
		}
	default:
		return &fwv1.PluginError{
			Code:    fwv1.ErrorCode_ERROR_CODE_INTERNAL,
			Message: err.Error(),
		}
	}
}

// toStatus converts an error to a gRPC status carrying the PluginError as a detail, so the manager
// receives structured information even for unary RPCs.
func toStatus(err error) error {
	if err == nil {
		return nil
	}

	pe := AsPluginError(err)
	st := status.New(grpcCodeFor(pe.GetCode()), pe.GetMessage())

	withDetails, detailErr := st.WithDetails(pe)
	if detailErr != nil {
		// Attaching details is best-effort: losing them must not turn a useful failure into an
		// unrelated marshalling error.
		return st.Err()
	}
	return withDetails.Err()
}

func grpcCodeFor(code fwv1.ErrorCode) codes.Code {
	switch code {
	case fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED:
		return codes.Unimplemented
	case fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return codes.InvalidArgument
	case fwv1.ErrorCode_ERROR_CODE_AUTHENTICATION_FAILED:
		return codes.Unauthenticated
	case fwv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return codes.PermissionDenied
	case fwv1.ErrorCode_ERROR_CODE_TIMEOUT:
		return codes.DeadlineExceeded
	case fwv1.ErrorCode_ERROR_CODE_CANCELED:
		return codes.Canceled
	case fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
		fwv1.ErrorCode_ERROR_CODE_OBJECT_STORE_FAILED:
		return codes.Unavailable
	case fwv1.ErrorCode_ERROR_CODE_TOOL_NOT_FOUND:
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}

// PluginErrorFrom extracts a PluginError from a gRPC status returned by a plugin, so core can act
// on the structured code rather than on a message string.
func PluginErrorFrom(err error) (*fwv1.PluginError, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return nil, false
	}
	for _, detail := range st.Details() {
		if pe, ok := detail.(*fwv1.PluginError); ok {
			return pe, true
		}
	}
	return nil, false
}
