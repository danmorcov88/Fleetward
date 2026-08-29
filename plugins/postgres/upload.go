package postgres

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// uploadClientTimeout bounds a single part transfer. It is generous because a part is tens of
// megabytes over a link the plugin does not control, and short because a transfer that has stalled
// must not hold a backup open indefinitely.
const uploadClientTimeout = 10 * time.Minute

// artifactUploader writes a stream of unknown length into a multipart upload core has already
// begun, using only the presigned grants in the request.
//
// It never holds the whole artifact: one part-sized buffer is reused for every part, so peak memory
// is the part size regardless of how large the backup is. The plugin holds no storage credential
// and cannot complete the upload itself — it returns the parts' receipts and core completes it
// (ADR-0021).
type artifactUploader struct {
	target *fwv1.ArtifactTarget
	client *http.Client
	// buf is the single reusable part buffer.
	buf []byte
	// onProgress is called after each part with the running byte total. A nil value disables it.
	onProgress func(bytesWritten int64) error
}

// newArtifactUploader validates the grant and allocates the part buffer.
func newArtifactUploader(target *fwv1.ArtifactTarget, onProgress func(int64) error) (*artifactUploader, error) {
	if target.GetObject().GetKey() == "" {
		return nil, sdk.InvalidArgument("target.object.key is required")
	}
	if len(target.GetPartUrls()) == 0 {
		// This is not a limitation of this plugin but of S3 itself: a single-shot PUT needs a
		// Content-Length, and the size of a pg_dump stream is not known until it has finished.
		return nil, sdk.InvalidArgument(
			"target.part_urls is required: an artifact streamed from a native tool has no known " +
				"size, and a single-shot presigned PUT cannot accept it")
	}
	partSize := target.GetPartSizeBytes()
	if partSize <= 0 {
		return nil, sdk.InvalidArgument("target.part_size_bytes must be positive")
	}
	// A part buffer is sized by core, so a hostile or broken value would otherwise be an allocation
	// the plugin makes on request.
	const maxPartSize = 512 << 20
	if partSize > maxPartSize {
		return nil, sdk.InvalidArgument("target.part_size_bytes of %d exceeds the %d limit", partSize, maxPartSize)
	}

	return &artifactUploader{
		target:     target,
		client:     &http.Client{Timeout: uploadClientTimeout},
		buf:        make([]byte, partSize),
		onProgress: onProgress,
	}, nil
}

// upload streams r into the presigned parts and returns the receipts core needs to complete the
// upload, along with the total number of bytes written.
//
// A failure part-way through leaves parts already written in the store. That is core's to clean up
// with AbortMultipartUpload, and it is why nothing this function does can produce a visible object:
// an artifact only exists once core completes the upload, so a half-uploaded backup can never be
// mistaken for a whole one.
func (u *artifactUploader) upload(ctx context.Context, r io.Reader) ([]*fwv1.UploadedPart, int64, error) {
	var (
		parts []*fwv1.UploadedPart
		total int64
	)

	for number, grant := range u.target.GetPartUrls() {
		// A short read (io.ErrUnexpectedEOF) is the final part, which alone may be smaller than
		// part_size_bytes; a plain io.EOF means the stream ended exactly on a part boundary and
		// there is nothing left to write. Neither is a failure.
		n, readErr := io.ReadFull(r, u.buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil, total, readErr
		}
		if n == 0 {
			break
		}

		etag, err := u.putPart(ctx, grant, u.buf[:n])
		if err != nil {
			return nil, total, err
		}

		total += int64(n)
		parts = append(parts, &fwv1.UploadedPart{
			PartNumber: int32(number + 1), //nolint:gosec // G115: bounded by len(part_urls), an int32 from the wire
			Etag:       etag,
			SizeBytes:  int64(n),
		})

		if u.onProgress != nil {
			if err := u.onProgress(total); err != nil {
				return nil, total, err
			}
		}

		if n < len(u.buf) {
			return parts, total, nil
		}
	}

	// Every grant is spent. If the stream still has data, the artifact is larger than core provided
	// capacity for, and completing the upload would silently truncate the backup.
	var probe [1]byte
	switch _, err := io.ReadFull(r, probe[:]); {
	case errors.Is(err, io.EOF):
	case err == nil:
		return nil, total, sdk.ObjectStoreFailed(
			"the artifact is larger than the %d bytes of upload capacity core granted; "+
				"raise FLEETWARD_OBJSTORE_PART_SIZE_BYTES on the control plane",
			int64(len(u.buf))*int64(len(u.target.GetPartUrls())))
	default:
		return nil, total, err
	}

	// An empty stream yields no parts and no bytes. Whether that is a failure depends on what
	// produced the stream, so the judgement is left to the caller.
	return parts, total, nil
}

// putPart writes one part and returns the ETag the store assigned it.
func (u *artifactUploader) putPart(ctx context.Context, grant *fwv1.PresignedURL, payload []byte) (string, error) {
	method := grant.GetMethod()
	if method == "" {
		method = http.MethodPut
	}

	req, err := http.NewRequestWithContext(ctx, method, grant.GetUrl(), bytes.NewReader(payload))
	if err != nil {
		// The URL is a bearer credential; only its redacted form may appear in an error.
		return "", sdk.ObjectStoreFailed("build the upload request for %s",
			sdk.SafeURL(grant.GetUrl())).WithCause(err)
	}
	// Explicit rather than inferred: this is the header whose absence makes the whole single-shot
	// upload path impossible, and an accidental chunked body would be rejected with a 411.
	req.ContentLength = int64(len(payload))
	for key, value := range grant.GetHeaders() {
		req.Header.Set(key, value)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", sdk.ObjectStoreFailed("upload a part to %s", sdk.SafeURL(grant.GetUrl())).WithCause(err)
	}
	defer func() {
		// Draining lets the connection be reused for the next part rather than being torn down.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", sdk.ObjectStoreFailed("object store rejected a part with %s: %s",
			resp.Status, trimStoreError(string(body)))
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		// Without an ETag core cannot complete the upload, and reporting success here would leave
		// a backup that appears to have been written but can never be assembled.
		return "", sdk.ObjectStoreFailed("object store returned no ETag for a part")
	}
	return etag, nil
}

// trimStoreError trims an object store's error body to something safe and short for a log line.
func trimStoreError(body string) string {
	const limit = 300
	body = strings.TrimSpace(body)
	if len(body) > limit {
		body = body[:limit] + "…"
	}
	return strings.Join(strings.Fields(body), " ")
}
