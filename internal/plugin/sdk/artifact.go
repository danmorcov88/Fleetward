package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Moving an artifact between a plugin and the object store.
//
// This is protocol, not convenience. ADR-0021 fixes how an artifact is written — core begins a
// multipart upload and presigns one grant per part, the plugin writes the parts and reports their
// receipts, core completes it — and ADR-0022 fixes what must happen before a restore touches
// anything: the bytes are confirmed to be the bytes that were written, or the artifact is blamed.
//
// Both halves live here rather than in a plugin because a second copy of either is a second place
// for them to drift, and drift in the first one silently truncates a backup while drift in the
// second one silently verifies an artifact nobody checked.
//
// A plugin never receives a storage credential. Everything below goes through the presigned grants
// core supplied for exactly this call (ADR-0007), and a presigned URL is itself a bearer credential
// — so only SafeURL's redacted form of one may appear in an error or a log line.

const (
	// uploadClientTimeout bounds a single part transfer. Generous, because a part is tens of
	// megabytes over a link the plugin does not control; bounded, because a stalled transfer must
	// not hold a backup open indefinitely.
	uploadClientTimeout = 10 * time.Minute

	// downloadTimeout bounds fetching one whole artifact, for the same two reasons.
	downloadTimeout = 30 * time.Minute

	// maxPartSize caps the buffer a plugin allocates on core's say-so. The part size arrives over
	// the wire, so without a ceiling a broken or hostile value is an allocation made on request.
	maxPartSize = 512 << 20
)

// ArtifactUploader writes a stream of unknown length into a multipart upload core has already
// begun, using only the presigned grants in the request.
//
// It never holds the whole artifact: one part-sized buffer is reused for every part, so peak memory
// is the part size regardless of how large the backup is. The plugin holds no storage credential
// and cannot complete the upload itself — it returns the parts' receipts and core completes it.
type ArtifactUploader struct {
	target *fwv1.ArtifactTarget
	client *http.Client
	// buf is the single reusable part buffer.
	buf []byte
	// onProgress is called after each part with the running byte total. A nil value disables it.
	onProgress func(bytesWritten int64) error
}

// NewArtifactUploader validates the grant and allocates the part buffer.
func NewArtifactUploader(target *fwv1.ArtifactTarget, onProgress func(int64) error) (*ArtifactUploader, error) {
	if target.GetObject().GetKey() == "" {
		return nil, InvalidArgument("target.object.key is required")
	}
	if len(target.GetPartUrls()) == 0 {
		// This is not a limitation of any one plugin but of S3 itself: a single-shot PUT needs a
		// Content-Length, and the size of a tool's output is not known until it has finished.
		return nil, InvalidArgument(
			"target.part_urls is required: an artifact streamed from a native tool has no known " +
				"size, and a single-shot presigned PUT cannot accept it")
	}
	partSize := target.GetPartSizeBytes()
	if partSize <= 0 {
		return nil, InvalidArgument("target.part_size_bytes must be positive")
	}
	if partSize > maxPartSize {
		return nil, InvalidArgument("target.part_size_bytes of %d exceeds the %d limit", partSize, maxPartSize)
	}

	return &ArtifactUploader{
		target:     target,
		client:     &http.Client{Timeout: uploadClientTimeout},
		buf:        make([]byte, partSize),
		onProgress: onProgress,
	}, nil
}

// Upload streams r into the presigned parts and returns the receipts core needs to complete the
// upload, along with the total number of bytes written.
//
// A failure part-way through leaves parts already written in the store. That is core's to clean up
// with AbortMultipartUpload, and it is why nothing this function does can produce a visible object:
// an artifact only exists once core completes the upload, so a half-uploaded backup can never be
// mistaken for a whole one.
func (u *ArtifactUploader) Upload(ctx context.Context, r io.Reader) ([]*fwv1.UploadedPart, int64, error) {
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
		return nil, total, ObjectStoreFailed(
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
func (u *ArtifactUploader) putPart(ctx context.Context, grant *fwv1.PresignedURL, payload []byte) (string, error) {
	method := grant.GetMethod()
	if method == "" {
		method = http.MethodPut
	}

	req, err := http.NewRequestWithContext(ctx, method, grant.GetUrl(), bytes.NewReader(payload))
	if err != nil {
		return "", ObjectStoreFailed("build the upload request for %s", SafeURL(grant.GetUrl())).WithCause(err)
	}
	// Explicit rather than inferred: this is the header whose absence makes the whole single-shot
	// upload path impossible, and an accidental chunked body would be rejected with a 411.
	req.ContentLength = int64(len(payload))
	for key, value := range grant.GetHeaders() {
		req.Header.Set(key, value)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", ObjectStoreFailed("upload a part to %s", SafeURL(grant.GetUrl())).WithCause(err)
	}
	defer func() {
		// Draining lets the connection be reused for the next part rather than being torn down.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", ObjectStoreFailed("object store rejected a part with %s: %s",
			resp.Status, TrimStoreError(string(body)))
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		// Without an ETag core cannot complete the upload, and reporting success here would leave
		// a backup that appears to have been written but can never be assembled.
		return "", ObjectStoreFailed("object store returned no ETag for a part")
	}
	return etag, nil
}

// FetchArtifact streams an artifact through its presigned download grant into dst, hashing as it
// goes, and confirms the result against the checksum recorded when the artifact was written.
//
// It returns only after the whole artifact has been written and the hash has matched, which is what
// makes ADR-0022's rule implementable: nothing is applied to a restore target until the bytes are
// known to be the bytes that were stored. A mismatch is ArtifactCorrupt — the one failure on this
// path that is evidence about the backup rather than about the machinery.
func FetchArtifact(ctx context.Context, artifact *fwv1.ArtifactSource, dst io.Writer, onProgress func(int64) error) (int64, error) {
	grant := artifact.GetDownloadUrl()
	method := grant.GetMethod()
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, grant.GetUrl(), nil)
	if err != nil {
		return 0, ObjectStoreFailed("build the download request for %s", SafeURL(grant.GetUrl())).WithCause(err)
	}
	for key, value := range grant.GetHeaders() {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, ObjectStoreFailed("download the artifact from %s", SafeURL(grant.GetUrl())).WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return 0, ObjectStoreFailed("object store rejected the download with %s: %s",
			resp.Status, TrimStoreError(string(body)))
	}

	hasher := sha256.New()
	size, err := CopyWithProgress(dst, io.TeeReader(resp.Body, hasher), onProgress)
	if err != nil {
		return size, ObjectStoreFailed("read the artifact").WithCause(err)
	}

	if err := VerifyChecksum(artifact.GetChecksum(), hasher.Sum(nil), size); err != nil {
		return size, err
	}
	if declared := artifact.GetSizeBytes(); declared > 0 && declared != size {
		return size, ArtifactCorrupt(
			"the artifact is %d bytes but %d were recorded when it was written", size, declared)
	}
	return size, nil
}

// SelectSingleArtifact picks the one base artifact a non-incremental method restores from.
//
// A method that declares no incremental support must refuse a chain rather than silently restore
// the first element of it, which would produce a database missing everything after the base.
func SelectSingleArtifact(artifacts []*fwv1.ArtifactSource, methodID string) (*fwv1.ArtifactSource, error) {
	var bases []*fwv1.ArtifactSource
	for _, a := range artifacts {
		switch a.GetRole() {
		case fwv1.ArtifactRole_ARTIFACT_ROLE_UNSPECIFIED, fwv1.ArtifactRole_ARTIFACT_ROLE_BASE:
			bases = append(bases, a)
		default:
			return nil, InvalidArgument(
				"the %s method restores from a single base artifact; %s was supplied", methodID, a.GetRole())
		}
	}

	switch len(bases) {
	case 0:
		return nil, InvalidArgument("no artifact to restore from")
	case 1:
		if bases[0].GetDownloadUrl().GetUrl() == "" {
			return nil, InvalidArgument("the artifact carries no download grant")
		}
		return bases[0], nil
	default:
		return nil, InvalidArgument(
			"the %s method restores from a single artifact; %d were supplied", methodID, len(bases))
	}
}

// VerifyChecksum compares what arrived against what was recorded when the artifact was written.
//
// This is the check that separates "the backup does not restore" from "the backup is not the bytes
// we stored". A missing checksum is refused rather than skipped: verification whose evidence chain
// has a hole in it reports a confidence it has not earned.
func VerifyChecksum(expected *fwv1.Checksum, actual []byte, size int64) error {
	if expected == nil || expected.GetValue() == "" {
		return InvalidArgument(
			"the artifact carries no checksum, so it cannot be confirmed to be the one that was written")
	}
	switch expected.GetAlgorithm() {
	case fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED,
		fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256:
	default:
		return InvalidArgument("checksum algorithm %s is not implemented; this plugin computes SHA-256",
			expected.GetAlgorithm())
	}

	got := hex.EncodeToString(actual)
	if !strings.EqualFold(got, strings.TrimSpace(expected.GetValue())) {
		return ArtifactCorrupt(
			"the artifact does not match its checksum: %d bytes hash to %s, but %s was recorded when "+
				"it was written", size, got, expected.GetValue())
	}
	return nil
}

// CopyWithProgress streams src into dst, reporting the running total at part-sized intervals.
func CopyWithProgress(dst io.Writer, src io.Reader, onProgress func(int64) error) (int64, error) {
	const (
		bufferSize  = 1 << 20
		reportEvery = 64 << 20
	)

	buf := make([]byte, bufferSize)
	var total, reported int64

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return total, writeErr
			}
			total += int64(n)
			if onProgress != nil && total-reported >= reportEvery {
				reported = total
				if err := onProgress(total); err != nil {
					return total, err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// TrimStoreError trims an object store's error body to something safe and short for a log line.
func TrimStoreError(body string) string {
	const limit = 300
	body = strings.TrimSpace(body)
	if len(body) > limit {
		body = body[:limit] + "…"
	}
	return strings.Join(strings.Fields(body), " ")
}
