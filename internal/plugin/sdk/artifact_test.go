package sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// The artifact transfer protocol's own tests. They moved here from the PostgreSQL plugin when the
// protocol did (slice B2): a second engine needs the same upload and the same checksum, and a
// protocol with two implementations is a protocol with two behaviours.

// TestUploaderRequiresPartGrants pins the reason ADR-0021 exists: a streamed artifact of unknown
// size cannot go through a single-shot presigned PUT, so a target without part grants is refused
// rather than attempted.
func TestUploaderRequiresPartGrants(t *testing.T) {
	_, err := NewArtifactUploader(&fwv1.ArtifactTarget{
		Object:            &fwv1.ObjectRef{Bucket: "b", Key: "k"},
		UploadUrl:         &fwv1.PresignedURL{Url: "https://example.invalid/put", Method: http.MethodPut},
		PartUrls:          nil,
		ChecksumAlgorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
	}, nil)
	if err == nil {
		t.Fatal("NewArtifactUploader() accepted a target with no part grants")
	}
	if !strings.Contains(err.Error(), "part_urls") {
		t.Errorf("error %q should explain that part grants are required", err)
	}
}

// TestUploadSplitsAndReportsParts drives the uploader against a stand-in object store and checks the
// two things core depends on: every byte arrives, and every part comes back with a receipt.
func TestUploadSplitsAndReportsParts(t *testing.T) {
	const partSize = 8

	received := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength < 0 {
			// This is the failure the whole multipart design exists to avoid; asserting it here
			// means a regression to a chunked body is caught by a unit test.
			http.Error(w, "chunked bodies are rejected by object stores", http.StatusLengthRequired)
			return
		}
		body := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received[r.URL.Query().Get("partNumber")] = body
		w.Header().Set("ETag", `"etag-`+r.URL.Query().Get("partNumber")+`"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	target := &fwv1.ArtifactTarget{
		Object:        &fwv1.ObjectRef{Bucket: "b", Key: "k"},
		PartSizeBytes: partSize,
	}
	for n := 1; n <= 4; n++ {
		target.PartUrls = append(target.PartUrls, &fwv1.PresignedURL{
			Url:    server.URL + "/?partNumber=" + strconv.Itoa(n),
			Method: http.MethodPut,
		})
	}

	uploader, err := NewArtifactUploader(target, nil)
	if err != nil {
		t.Fatalf("NewArtifactUploader() error = %v", err)
	}

	// 20 bytes over an 8-byte part size: two full parts and a short final one.
	payload := strings.Repeat("a", 20)
	parts, total, err := uploader.Upload(t.Context(), strings.NewReader(payload))
	if err != nil {
		t.Fatalf("upload() error = %v", err)
	}

	if total != int64(len(payload)) {
		t.Errorf("total = %d, want %d", total, len(payload))
	}
	if len(parts) != 3 {
		t.Fatalf("uploaded %d parts, want 3", len(parts))
	}
	wantSizes := []int64{8, 8, 4}
	for i, part := range parts {
		if part.GetPartNumber() != int32(i+1) {
			t.Errorf("part %d has number %d", i, part.GetPartNumber())
		}
		if part.GetSizeBytes() != wantSizes[i] {
			t.Errorf("part %d size = %d, want %d", i+1, part.GetSizeBytes(), wantSizes[i])
		}
		if part.GetEtag() == "" {
			t.Errorf("part %d came back without an ETag; core could not complete the upload", i+1)
		}
	}

	var reassembled strings.Builder
	for n := 1; n <= 3; n++ {
		reassembled.Write(received[strconv.Itoa(n)])
	}
	if reassembled.String() != payload {
		t.Errorf("reassembled artifact = %q, want %q", reassembled.String(), payload)
	}
}

// TestUploadRefusesToTruncate asserts the boundary that would otherwise produce a silently short
// backup: more data than the granted parts can hold is an error, never a truncation.
func TestUploadRefusesToTruncate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	uploader, err := NewArtifactUploader(&fwv1.ArtifactTarget{
		Object:        &fwv1.ObjectRef{Bucket: "b", Key: "k"},
		PartSizeBytes: 4,
		PartUrls:      []*fwv1.PresignedURL{{Url: server.URL, Method: http.MethodPut}},
	}, nil)
	if err != nil {
		t.Fatalf("NewArtifactUploader() error = %v", err)
	}

	if _, _, err := uploader.Upload(t.Context(), strings.NewReader("more than four bytes")); err == nil {
		t.Fatal("upload() truncated the artifact instead of failing")
	}
}

// TestVerifyChecksumBlamesTheArtifact is what makes slice A6 possible: a corrupted artifact has to
// be distinguishable from a flaky download, because one is data loss and the other is a network.
func TestVerifyChecksumBlamesTheArtifact(t *testing.T) {
	payload := []byte("fleetward artifact bytes")
	sum := sha256.Sum256(payload)
	correct := hex.EncodeToString(sum[:])

	t.Run("a matching checksum passes", func(t *testing.T) {
		err := VerifyChecksum(&fwv1.Checksum{
			Algorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
			Value:     correct,
		}, sum[:], int64(len(payload)))
		if err != nil {
			t.Fatalf("a matching checksum was rejected: %v", err)
		}
	})

	t.Run("uppercase is still a match", func(t *testing.T) {
		err := VerifyChecksum(&fwv1.Checksum{Value: strings.ToUpper(correct)}, sum[:], int64(len(payload)))
		if err != nil {
			t.Fatalf("case alone made a checksum mismatch: %v", err)
		}
	})

	t.Run("a mismatch is reported as a corrupt artifact", func(t *testing.T) {
		err := VerifyChecksum(&fwv1.Checksum{
			Algorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
			Value:     strings.Repeat("0", 64),
		}, sum[:], int64(len(payload)))
		if err == nil {
			t.Fatal("a mismatched checksum was accepted")
		}
		if !IsArtifactCorrupt(AsPluginError(err)) {
			t.Errorf("the error does not blame the artifact, so core would report it as inconclusive: %v", err)
		}
	})

	t.Run("no checksum is refused rather than skipped", func(t *testing.T) {
		if err := VerifyChecksum(nil, sum[:], int64(len(payload))); err == nil {
			t.Fatal("an artifact with no checksum was accepted; there would be no evidence chain")
		}
	})
}

func TestSelectArtifact(t *testing.T) {
	base := func() *fwv1.ArtifactSource {
		return &fwv1.ArtifactSource{
			Role:        fwv1.ArtifactRole_ARTIFACT_ROLE_BASE,
			DownloadUrl: &fwv1.PresignedURL{Url: "https://store.example/artifact?sig=x"},
		}
	}

	tests := []struct {
		name    string
		in      []*fwv1.ArtifactSource
		wantErr bool
	}{
		{"one base artifact", []*fwv1.ArtifactSource{base()}, false},
		{"none", nil, true},
		// Quietly using the first would restore less than was asked for, which is the failure this
		// product exists to detect rather than commit.
		{"two", []*fwv1.ArtifactSource{base(), base()}, true},
		{
			name: "a WAL segment",
			in: []*fwv1.ArtifactSource{{
				Role:        fwv1.ArtifactRole_ARTIFACT_ROLE_LOG,
				DownloadUrl: &fwv1.PresignedURL{Url: "https://store.example/wal"},
			}},
			wantErr: true,
		},
		{"no download grant", []*fwv1.ArtifactSource{{Role: fwv1.ArtifactRole_ARTIFACT_ROLE_BASE}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SelectSingleArtifact(tc.in, "test-method")
			if tc.wantErr && err == nil {
				t.Fatal("expected a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("selectArtifact: %v", err)
			}
		})
	}
}
