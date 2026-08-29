// Package objstore abstracts S3-compatible storage for backup artifacts (ADR-0007).
//
// The interface exists so that a future filesystem, GCS-native, or tape-adjacent backend can be
// added without touching the backup and verification services. It also enforces the rule that
// matters most here: plugins receive presigned URLs, never storage credentials.
package objstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
	Metadata     map[string]string
}

// PutOptions tunes a single upload.
type PutOptions struct {
	ContentType string
	Metadata    map[string]string
	// PartSize overrides the store's default multipart chunk size. Zero uses the default.
	PartSize int64
}

// PresignedURL is a scoped, time-limited grant to perform one HTTP operation on one object.
type PresignedURL struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// MultipartUpload is an upload in progress, together with the grants a plugin needs to write its
// parts. It exists because an artifact streamed out of a native tool has no known size, and an S3
// PUT requires Content-Length — see ADR-0021.
//
// An upload left neither completed nor aborted keeps its uploaded parts occupying storage and
// billing for it, so every caller must reach one of the two terminal calls on every path.
type MultipartUpload struct {
	Key      string
	UploadID string
	// PartSize is how many bytes every part except the last must contain.
	PartSize int64
	// Parts are the presigned PUT grants, in part order. Part number n is Parts[n-1].
	Parts []PresignedURL
}

// Capacity is the largest artifact this upload can hold.
func (m MultipartUpload) Capacity() int64 { return m.PartSize * int64(len(m.Parts)) }

// CompletedPart is one part's receipt, as reported by whoever uploaded it.
type CompletedPart struct {
	// PartNumber is 1-based and must match the grant the part was written through.
	PartNumber int
	// ETag exactly as the store returned it. It is passed back verbatim: an S3 ETag is quoted, and
	// stripping the quotes makes CompleteMultipartUpload fail with an unhelpful signature error.
	ETag string
}

// ObjectStore is the storage abstraction used by the backup and verification services.
// Implementations must be safe for concurrent use.
type ObjectStore interface {
	// Bucket returns the bucket artifacts are written to.
	Bucket() string

	// EnsureBucket creates the configured bucket if it does not exist. Called once at startup so
	// that a fresh MinIO in the dev stack works without manual setup.
	EnsureBucket(ctx context.Context) error

	// Put uploads an object. A size of -1 means unknown, which forces a streaming multipart upload.
	Put(ctx context.Context, key string, r io.Reader, size int64, opts PutOptions) (ObjectInfo, error)

	// Get opens an object for reading. The caller must close the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)

	// Stat returns metadata without transferring the object, returning ErrNotFound if absent.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes an object. Deleting an object that does not exist is not an error.
	Delete(ctx context.Context, key string) error

	// List returns objects under a prefix, up to limit. A limit of zero means no cap.
	List(ctx context.Context, prefix string, limit int) ([]ObjectInfo, error)

	// PresignPut issues a time-limited grant to upload one object. This is how a plugin writes an
	// artifact without ever holding a storage credential.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (PresignedURL, error)

	// PresignGet issues a time-limited grant to download one object, used when restoring.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (PresignedURL, error)

	// CreateMultipartUpload begins a multipart upload and presigns partCount part grants. This is
	// how an artifact of unknown size is written by a plugin that holds no storage credential.
	//
	// Callers must eventually call CompleteMultipartUpload or AbortMultipartUpload.
	CreateMultipartUpload(ctx context.Context, key string, partCount int, ttl time.Duration) (MultipartUpload, error)

	// CompleteMultipartUpload assembles the uploaded parts into one object. Parts may arrive in any
	// order; implementations sort them by part number.
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) (ObjectInfo, error)

	// AbortMultipartUpload discards an incomplete upload and the parts already written. Aborting an
	// upload that no longer exists is not an error, so it is safe in a deferred cleanup.
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error

	// HealthCheck reports whether the store is reachable and the bucket accessible.
	HealthCheck(ctx context.Context) error

	// Close releases resources held by the store.
	Close() error
}

// ArtifactKey builds the canonical object key for a backup artifact.
//
// Tenant comes first so that a bucket policy or lifecycle rule can be scoped to one tenant by
// prefix, and so that listing one tenant's artifacts never scans another's.
func ArtifactKey(tenantID, instanceID, backupID, filename string) string {
	return "tenants/" + tenantID +
		"/instances/" + instanceID +
		"/backups/" + backupID +
		"/" + filename
}

// defaultTransport is shared by store implementations so that connection pooling is not silently
// lost by constructing a fresh http.Client per operation.
func defaultTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Something has replaced the default transport. Fall back rather than panicking; the
		// tuning below is an optimization, not a requirement.
		return &http.Transport{}
	}
	t := base.Clone()
	t.MaxIdleConnsPerHost = 32
	// Artifact transfers are long-lived; a short idle timeout would churn connections mid-backup.
	t.IdleConnTimeout = 90 * time.Second
	return t
}
