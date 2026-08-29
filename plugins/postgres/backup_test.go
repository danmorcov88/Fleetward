package postgres

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

func TestDumpArgs(t *testing.T) {
	creds := &fwv1.Credentials{
		Host:     "db.internal",
		Port:     5433,
		Username: "backup_operator",
		Password: "s3cr3t",
		Database: "app",
	}

	tests := []struct {
		name       string
		creds      *fwv1.Credentials
		database   string
		format     string
		snapshotID string
		want       []string
	}{
		{
			name:       "custom format with a snapshot",
			creds:      creds,
			database:   "app",
			format:     formatCustom,
			snapshotID: "00000003-0000001B-1",
			want: []string{
				"--host=db.internal", "--port=5433", "--username=backup_operator",
				"--dbname=app", "--format=custom", "--no-password",
				"--snapshot=00000003-0000001B-1",
			},
		},
		{
			name:     "plain format without a snapshot",
			creds:    creds,
			database: "reporting",
			format:   formatPlain,
			want: []string{
				"--host=db.internal", "--port=5433", "--username=backup_operator",
				"--dbname=reporting", "--format=plain", "--no-password",
			},
		},
		{
			name:     "a missing port falls back to the engine default",
			creds:    &fwv1.Credentials{Host: "db.internal", Username: "u", Database: "app"},
			database: "app",
			format:   formatCustom,
			want: []string{
				"--host=db.internal", "--port=5432", "--username=u",
				"--dbname=app", "--format=custom", "--no-password",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dumpArgs(tc.creds, tc.database, tc.format, tc.snapshotID)
			if len(got) != len(tc.want) {
				t.Fatalf("dumpArgs() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("arg %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDumpArgsNeverCarryThePassword is the guard behind the rule that matters most here: anything on
// argv is visible to every user on the host through ps, and this process dumps a production
// database.
func TestDumpArgsNeverCarryThePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	creds := &fwv1.Credentials{
		Host: "db.internal", Port: 5432, Username: "u", Password: password, Database: "app",
	}

	joined := strings.Join(dumpArgs(creds, "app", formatCustom, "snap"), " ")
	if strings.Contains(joined, password) {
		t.Fatalf("dumpArgs() leaked the password onto the command line: %s", joined)
	}
	for _, arg := range dumpArgs(creds, "app", formatCustom, "snap") {
		if strings.Contains(strings.ToLower(arg), "password=") && !strings.HasPrefix(arg, "--no-password") {
			t.Errorf("argument %q looks like it carries a password", arg)
		}
	}
}

func TestDumpEnv(t *testing.T) {
	// The parent environment deliberately contains PG variables that would redirect or downgrade the
	// backup if they were inherited.
	parent := []string{
		"PATH=/usr/bin",
		"PGDATABASE=someone-elses-database",
		"PGHOST=someone-elses-host",
		"PGSSLMODE=disable",
		"HOME=/home/fleetward",
	}
	creds := &fwv1.Credentials{Host: "db", Port: 5432, Username: "u", Password: "pw", Database: "app"}

	env, err := dumpEnv(parent, creds, tlsMaterial{})
	if err != nil {
		t.Fatalf("dumpEnv() error = %v", err)
	}

	got := envMap(env)
	if _, ok := got["PGDATABASE"]; ok {
		t.Error("PGDATABASE was inherited from the parent; a backup would target the wrong database")
	}
	if got["PGHOST"] != "" {
		t.Error("PGHOST was inherited from the parent")
	}
	if got["PATH"] != "/usr/bin" || got["HOME"] != "/home/fleetward" {
		t.Error("non-PG parent variables should be preserved")
	}
	if got["PGPASSWORD"] != "pw" {
		t.Errorf("PGPASSWORD = %q, want the request's password", got["PGPASSWORD"])
	}
	if got["PGAPPNAME"] != "fleetward" {
		t.Errorf("PGAPPNAME = %q, want fleetward", got["PGAPPNAME"])
	}
	if got["PGSSLMODE"] != "disable" {
		t.Errorf("PGSSLMODE = %q, want disable for a connection without TLS", got["PGSSLMODE"])
	}
}

func TestSSLMode(t *testing.T) {
	tests := []struct {
		name     string
		settings *fwv1.TLSSettings
		want     string
		wantErr  bool
	}{
		{name: "absent", settings: nil, want: "disable"},
		{name: "disabled", settings: &fwv1.TLSSettings{Enabled: false}, want: "disable"},
		{
			name:     "enabled without verification",
			settings: &fwv1.TLSSettings{Enabled: true, InsecureSkipVerify: true},
			want:     "require",
		},
		{
			name:     "enabled with a ca",
			settings: &fwv1.TLSSettings{Enabled: true, CaPem: []byte("-----BEGIN CERTIFICATE-----")},
			want:     "verify-full",
		},
		{
			name: "an expected server name steps down to chain verification",
			settings: &fwv1.TLSSettings{
				Enabled:    true,
				CaPem:      []byte("-----BEGIN CERTIFICATE-----"),
				ServerName: "primary.internal",
			},
			want: "verify-ca",
		},
		{
			// Refused rather than quietly downgraded: libpq cannot verify without a root
			// certificate, and a backup tool that silently stops verifying is the regression this
			// project exists to catch elsewhere.
			name:     "enabled with nothing to verify against",
			settings: &fwv1.TLSSettings{Enabled: true},
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sslMode(tc.settings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("sslMode() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("sslMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveDatabase(t *testing.T) {
	tests := []struct {
		name      string
		creds     *fwv1.Credentials
		requested []string
		want      string
		wantErr   bool
	}{
		{
			name:  "empty falls back to the connection's database",
			creds: &fwv1.Credentials{Database: "app"},
			want:  "app",
		},
		{
			name:      "one named database",
			creds:     &fwv1.Credentials{Database: "app"},
			requested: []string{"reporting"},
			want:      "reporting",
		},
		{
			name:    "no database anywhere",
			creds:   &fwv1.Credentials{},
			wantErr: true,
		},
		{
			// Refused rather than silently reduced to the first: a backup that quietly covered less
			// than was asked for is exactly the failure this product exists to detect.
			name:      "several databases",
			creds:     &fwv1.Credentials{Database: "app"},
			requested: []string{"app", "reporting"},
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDatabase(tc.creds, tc.requested)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveDatabase() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("resolveDatabase() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveFormat(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]string
		want    string
		wantErr bool
	}{
		{name: "default", options: nil, want: formatCustom},
		{name: "explicit custom", options: map[string]string{optionFormat: formatCustom}, want: formatCustom},
		{name: "plain", options: map[string]string{optionFormat: formatPlain}, want: formatPlain},
		{name: "directory is not offered", options: map[string]string{optionFormat: "directory"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveFormat(tc.options)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveFormat() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("resolveFormat() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveChecksumAlgorithm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested fwv1.ChecksumAlgorithm
		want      fwv1.ChecksumAlgorithm
		wantErr   bool
	}{
		{
			name:      "unspecified defaults to sha256",
			requested: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED,
			want:      fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
		},
		{
			name:      "sha256",
			requested: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
			want:      fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
		},
		{
			name:      "blake3 is not implemented",
			requested: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_BLAKE3,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveChecksumAlgorithm(tc.requested)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveChecksumAlgorithm() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("resolveChecksumAlgorithm() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBackupMethodsAreCoherent runs the SDK's own validation over the declared matrix, so a matrix
// that contradicts itself fails here rather than at a plugin's first scheduled backup.
func TestBackupMethodsAreCoherent(t *testing.T) {
	caps, err := New().Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if err := sdk.ValidateCapabilities(caps); err != nil {
		t.Fatalf("ValidateCapabilities() error = %v", err)
	}
	if !caps.GetSupportsOnlineBackup() {
		t.Error("supports_online_backup should be declared now that pg_dump is implemented")
	}
	method := sdk.DefaultBackupMethod(caps)
	if method == nil || method.GetId() != MethodPgDump {
		t.Fatalf("default backup method = %v, want %s", method, MethodPgDump)
	}
	if method.GetEnablesPitr() {
		t.Error("a logical dump is not a PITR baseline and must not claim to be one")
	}
}

// TestUploaderRequiresPartGrants pins the reason ADR-0021 exists: a streamed artifact of unknown
// size cannot go through a single-shot presigned PUT, so a target without part grants is refused
// rather than attempted.
func TestUploaderRequiresPartGrants(t *testing.T) {
	_, err := newArtifactUploader(&fwv1.ArtifactTarget{
		Object:            &fwv1.ObjectRef{Bucket: "b", Key: "k"},
		UploadUrl:         &fwv1.PresignedURL{Url: "https://example.invalid/put", Method: http.MethodPut},
		PartUrls:          nil,
		ChecksumAlgorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
	}, nil)
	if err == nil {
		t.Fatal("newArtifactUploader() accepted a target with no part grants")
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

	uploader, err := newArtifactUploader(target, nil)
	if err != nil {
		t.Fatalf("newArtifactUploader() error = %v", err)
	}

	// 20 bytes over an 8-byte part size: two full parts and a short final one.
	payload := strings.Repeat("a", 20)
	parts, total, err := uploader.upload(t.Context(), strings.NewReader(payload))
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

	uploader, err := newArtifactUploader(&fwv1.ArtifactTarget{
		Object:        &fwv1.ObjectRef{Bucket: "b", Key: "k"},
		PartSizeBytes: 4,
		PartUrls:      []*fwv1.PresignedURL{{Url: server.URL, Method: http.MethodPut}},
	}, nil)
	if err != nil {
		t.Fatalf("newArtifactUploader() error = %v", err)
	}

	if _, _, err := uploader.upload(t.Context(), strings.NewReader("more than four bytes")); err == nil {
		t.Fatal("upload() truncated the artifact instead of failing")
	}
}

func TestBoundedBufferKeepsTheTail(t *testing.T) {
	b := &boundedBuffer{limit: 10}
	for _, chunk := range []string{"aaaaa", "bbbbb", "ccccc"} {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if got := b.String(); got != "bbbbbccccc" {
		t.Errorf("String() = %q, want the last 10 bytes", got)
	}

	// A single write larger than the limit must also be trimmed rather than accepted whole.
	b = &boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := b.String(); got != "6789" {
		t.Errorf("String() = %q, want 6789", got)
	}
}

func TestLastLines(t *testing.T) {
	input := "one\ntwo\nthree\nfour\n"
	if got := lastLines(input, 2); got != "three; four" {
		t.Errorf("lastLines() = %q", got)
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines() = %q", got)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			out[name] = value
		}
	}
	return out
}
