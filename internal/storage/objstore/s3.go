package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/danmorcov88/fleetward/internal/config"
)

const (
	// maxMultipartParts is the S3 protocol ceiling on parts in one upload.
	maxMultipartParts = 10000
	// MinPartSizeBytes is the S3 floor on every part of a multipart upload except the last.
	//
	// It is enforced when the upload is completed, not when a part is written, so a part size below
	// it produces a backup that streams happily for an hour and then fails at the very end. Refusing
	// the configuration at startup turns that into an immediate, fixable error.
	MinPartSizeBytes = 5 << 20
	// artifactContentType is set on every artifact. Backups are opaque bytes whatever the engine
	// produced them, so a single type keeps core out of the business of knowing dump formats.
	artifactContentType = "application/octet-stream"
)

// S3Store is the S3-compatible ObjectStore implementation, backed by minio-go. It serves MinIO in
// the development stack and AWS S3, GCS, R2, or Ceph in production without code changes.
type S3Store struct {
	client     *minio.Client
	bucket     string
	region     string
	presignTTL time.Duration
	partSize   int64
}

var _ ObjectStore = (*S3Store)(nil)

// NewS3Store connects to the configured endpoint. It does not create the bucket; call
// EnsureBucket for that.
func NewS3Store(cfg config.ObjStoreConfig) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("objstore: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("objstore: bucket is required")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:    cfg.UseSSL,
		Region:    cfg.Region,
		Transport: defaultTransport(),
	})
	if err != nil {
		// minio.New embeds the endpoint but never the credentials in its errors, so wrapping is safe.
		return nil, fmt.Errorf("objstore: create client for %s: %w", cfg.Endpoint, err)
	}

	partSize := cfg.PartSizeBytes
	if partSize <= 0 {
		partSize = 64 << 20
	}
	if partSize < MinPartSizeBytes {
		return nil, fmt.Errorf(
			"objstore: part size %d is below the %d-byte minimum S3 requires for every part but the last",
			partSize, MinPartSizeBytes)
	}

	return &S3Store{
		client:     client,
		bucket:     cfg.Bucket,
		region:     cfg.Region,
		presignTTL: cfg.PresignTTL,
		partSize:   partSize,
	}, nil
}

// Bucket implements ObjectStore.
func (s *S3Store) Bucket() string { return s.bucket }

// EnsureBucket creates the bucket if it is absent.
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("objstore: check bucket %q: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: s.region}); err != nil {
		// A concurrent control-plane replica may have won the race; that is a success for us.
		if exists, checkErr := s.client.BucketExists(ctx, s.bucket); checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("objstore: create bucket %q: %w", s.bucket, err)
	}
	return nil
}

// Put implements ObjectStore.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64, opts PutOptions) (ObjectInfo, error) {
	// s.partSize is guaranteed positive by NewS3Store, so partSize is positive on both branches
	// and the conversion below cannot wrap.
	partSize := opts.PartSize
	if partSize <= 0 {
		partSize = s.partSize
	}

	info, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  opts.ContentType,
		UserMetadata: opts.Metadata,
		PartSize:     uint64(partSize), //nolint:gosec // G115: positive by the invariant above
	})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objstore: put %q: %w", key, err)
	}

	return ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		LastModified: info.LastModified,
		ContentType:  opts.ContentType,
		Metadata:     opts.Metadata,
	}, nil
}

// Get implements ObjectStore.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("objstore: get %q: %w", key, err)
	}

	// GetObject is lazy: it does not contact the server until the first read or Stat, so a missing
	// object surfaces here rather than at construction.
	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ObjectInfo{}, fmt.Errorf("objstore: get %q: %w", key, ErrNotFound)
		}
		return nil, ObjectInfo{}, fmt.Errorf("objstore: stat %q: %w", key, err)
	}

	return obj, objectInfoFrom(stat), nil
}

// Stat implements ObjectStore.
func (s *S3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	stat, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, fmt.Errorf("objstore: stat %q: %w", key, ErrNotFound)
		}
		return ObjectInfo{}, fmt.Errorf("objstore: stat %q: %w", key, err)
	}
	return objectInfoFrom(stat), nil
}

// Delete implements ObjectStore.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("objstore: delete %q: %w", key, err)
	}
	return nil
}

// List implements ObjectStore.
func (s *S3Store) List(ctx context.Context, prefix string, limit int) ([]ObjectInfo, error) {
	// The listing context is cancelled on return so minio-go's producer goroutine cannot outlive
	// an early exit from the loop below.
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var out []ObjectInfo
	for obj := range s.client.ListObjects(listCtx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("objstore: list %q: %w", prefix, obj.Err)
		}
		out = append(out, ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			ETag:         obj.ETag,
			LastModified: obj.LastModified,
			ContentType:  obj.ContentType,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// PresignPut implements ObjectStore.
func (s *S3Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (PresignedURL, error) {
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	signed, err := s.client.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return PresignedURL{}, fmt.Errorf("objstore: presign put %q: %w", key, err)
	}
	return PresignedURL{
		URL:       signed.String(),
		Method:    http.MethodPut,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// PresignGet implements ObjectStore.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (PresignedURL, error) {
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	signed, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return PresignedURL{}, fmt.Errorf("objstore: presign get %q: %w", key, err)
	}
	return PresignedURL{
		URL:       signed.String(),
		Method:    http.MethodGet,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// CreateMultipartUpload implements ObjectStore.
func (s *S3Store) CreateMultipartUpload(ctx context.Context, key string, partCount int, ttl time.Duration) (MultipartUpload, error) {
	if partCount < 1 || partCount > maxMultipartParts {
		return MultipartUpload{}, fmt.Errorf("objstore: part count %d is outside 1..%d", partCount, maxMultipartParts)
	}
	if ttl <= 0 {
		ttl = s.presignTTL
	}

	core := minio.Core{Client: s.client}
	uploadID, err := core.NewMultipartUpload(ctx, s.bucket, key, minio.PutObjectOptions{
		ContentType: artifactContentType,
	})
	if err != nil {
		return MultipartUpload{}, fmt.Errorf("objstore: begin multipart upload of %q: %w", key, err)
	}

	upload := MultipartUpload{
		Key:      key,
		UploadID: uploadID,
		PartSize: s.partSize,
		Parts:    make([]PresignedURL, 0, partCount),
	}
	expiresAt := time.Now().Add(ttl)

	for n := 1; n <= partCount; n++ {
		signed, err := s.client.Presign(ctx, http.MethodPut, s.bucket, key, ttl, url.Values{
			"uploadId":   []string{uploadID},
			"partNumber": []string{strconv.Itoa(n)},
		})
		if err != nil {
			// The upload is useless without a full set of grants, and leaving it open would keep
			// billing for whatever was already written.
			_ = core.AbortMultipartUpload(context.WithoutCancel(ctx), s.bucket, key, uploadID)
			return MultipartUpload{}, fmt.Errorf("objstore: presign part %d of %q: %w", n, key, err)
		}
		upload.Parts = append(upload.Parts, PresignedURL{
			URL:       signed.String(),
			Method:    http.MethodPut,
			ExpiresAt: expiresAt,
		})
	}

	return upload, nil
}

// CompleteMultipartUpload implements ObjectStore.
func (s *S3Store) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) (ObjectInfo, error) {
	if len(parts) == 0 {
		return ObjectInfo{}, fmt.Errorf("objstore: complete %q: no parts were uploaded", key)
	}

	complete := make([]minio.CompletePart, 0, len(parts))
	for _, part := range parts {
		if part.PartNumber < 1 || part.ETag == "" {
			return ObjectInfo{}, fmt.Errorf("objstore: complete %q: part %d has no usable receipt", key, part.PartNumber)
		}
		complete = append(complete, minio.CompletePart{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	// S3 requires ascending part numbers and rejects the whole upload otherwise. Sorting here means
	// a caller that collected receipts concurrently does not have to know that.
	slices.SortFunc(complete, func(a, b minio.CompletePart) int { return a.PartNumber - b.PartNumber })

	core := minio.Core{Client: s.client}
	if _, err := core.CompleteMultipartUpload(ctx, s.bucket, key, uploadID, complete,
		minio.PutObjectOptions{}); err != nil {
		return ObjectInfo{}, fmt.Errorf("objstore: complete multipart upload of %q: %w", key, err)
	}

	// CompleteMultipartUpload's own response carries no size, and the size is what core persists as
	// the backup's on-disk footprint, so it is read back from the assembled object.
	return s.Stat(ctx, key)
}

// AbortMultipartUpload implements ObjectStore.
func (s *S3Store) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	core := minio.Core{Client: s.client}
	if err := core.AbortMultipartUpload(ctx, s.bucket, key, uploadID); err != nil && !isNotFound(err) {
		return fmt.Errorf("objstore: abort multipart upload of %q: %w", key, err)
	}
	return nil
}

// HealthCheck implements ObjectStore.
func (s *S3Store) HealthCheck(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("objstore: unreachable: %w", err)
	}
	if !exists {
		return fmt.Errorf("objstore: bucket %q does not exist", s.bucket)
	}
	return nil
}

// Close implements ObjectStore. minio-go holds no resources requiring explicit release.
func (s *S3Store) Close() error { return nil }

func objectInfoFrom(o minio.ObjectInfo) ObjectInfo {
	return ObjectInfo{
		Key:          o.Key,
		Size:         o.Size,
		ETag:         o.ETag,
		LastModified: o.LastModified,
		ContentType:  o.ContentType,
		Metadata:     o.UserMetadata,
	}
}

func isNotFound(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey" ||
		minio.ToErrorResponse(err).StatusCode == http.StatusNotFound
}
