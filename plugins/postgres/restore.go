package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// restoreTool loads a custom-format archive; psqlTool replays a plain SQL script. Which one a
	// given artifact needs is recorded in the backup's own metadata by the method that produced it,
	// never inferred from the object key — core names every artifact "artifact" precisely so that it
	// never has to learn an engine's file formats.
	restoreTool = "pg_restore"
	psqlTool    = "psql"

	// metadataFormat and metadataDatabase are the keys the pg_dump method writes into
	// BackupResult.metadata. Core stores that map verbatim and hands it back as
	// RestoreRequest.options.
	metadataFormat   = "format"
	metadataDatabase = "database"

	// downloadTimeout bounds fetching one artifact. Generous, because a large artifact crosses a
	// link the plugin does not control; bounded, because a stalled transfer must not hold a
	// sandbox open indefinitely.
	downloadTimeout = 30 * time.Minute

	// defaultRestoreTimeout bounds a run when the request carries no timeout of its own.
	defaultRestoreTimeout = 2 * time.Hour
)

// sandboxTemplate tells core how to stand up a throwaway PostgreSQL for verification.
//
// Everything engine-specific about a sandbox lives here rather than in core, which is what allows a
// new engine to arrive as a plugin with no core change (CLAUDE.md §4). Core generates the identity
// and this template says where it belongs, through the {{ .Username }}, {{ .Password }},
// {{ .Database }}, and {{ .Port }} placeholders (ADR-0020).
func sandboxTemplate() *fwv1.SandboxTemplate {
	return &fwv1.SandboxTemplate{
		ImageRepository: "postgres",
		// The major version of the server that produced the artifact, which is not necessarily the
		// version the instance runs today: an instance can be upgraded between a backup and its
		// verification, while the artifact still restores as whatever wrote it. Core resolves this
		// against the version recorded on the backup.
		TagTemplate: "{{ .Major }}",
		// Only reached when a backup recorded no version at all. Verifying against the wrong major
		// version is worse than not verifying, so this is a floor for old rows rather than a
		// convenience — and there is deliberately no fallback to "latest".
		DefaultTag: "16",
		//nolint:gosec // G101: these are template placeholders core fills at provisioning time,
		// which is precisely the arrangement ADR-0020 exists to guarantee — a plugin binary must
		// never carry a credential.
		Env: map[string]string{
			"POSTGRES_USER":     "{{ .Username }}",
			"POSTGRES_PASSWORD": "{{ .Password }}",
			"POSTGRES_DB":       "{{ .Database }}",
		},
		ContainerPort: 5432,
		// Scoped to TCP on purpose. initdb runs a temporary server on the unix socket before the
		// real one starts, and a probe that reaches it reports ready while the server that will
		// actually answer is still being restarted.
		ReadinessCommand: []string{
			"pg_isready",
			"-h", "127.0.0.1",
			"-p", "{{ .Port }}",
			"-U", "{{ .Username }}",
			"-d", "{{ .Database }}",
		},
		// Generous, because the first verification on a machine also pays for initdb on a cold
		// container filesystem.
		ReadinessTimeout: durationpb.New(3 * time.Minute),
	}
}

// Restore loads one artifact into a target instance and streams progress.
//
// Only a sandbox target is accepted in this slice. Restoring over a real instance is destructive
// and needs core's authorization and typed confirmation before a plugin should be willing to do it;
// refusing here means the capability matrix and the behaviour cannot drift apart in the meantime.
//
// The final emitted message is always terminal, for the same reason Backup's is: without one core
// cannot tell a success from a stream that died.
func (p *Plugin) Restore(ctx context.Context, req *fwv1.RestoreRequest, emit sdk.Emitter[*fwv1.RestoreProgress]) error {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultRestoreTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := runRestore(runCtx, req, emit)
	if err != nil {
		return emit(&fwv1.RestoreProgress{
			RestoreId: req.GetRestoreId(),
			Phase:     fwv1.JobPhase_JOB_PHASE_FAILED,
			Error:     sdk.AsPluginError(err),
		})
	}

	return emit(&fwv1.RestoreProgress{
		RestoreId: req.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_COMPLETED,
		Message:   "restore complete",
		Result:    result,
	})
}

// runRestore performs the whole run, leaving the caller to turn an error into the terminal message.
func runRestore(ctx context.Context, req *fwv1.RestoreRequest, emit sdk.Emitter[*fwv1.RestoreProgress]) (*fwv1.RestoreResult, error) {
	started := time.Now()

	if id := req.GetMethodId(); id != "" && id != MethodPgDump {
		return nil, sdk.InvalidArgument("unknown restore method %q; this plugin implements %q", id, MethodPgDump)
	}
	if req.GetPointInTime().IsValid() {
		return nil, sdk.Unsupported(
			"point-in-time restore needs a WAL-based method; the %s method restores only to the "+
				"artifact's own consistency point", MethodPgDump)
	}

	creds, err := restoreTargetCredentials(req.GetTarget())
	if err != nil {
		return nil, err
	}
	artifact, err := selectArtifact(req.GetArtifacts())
	if err != nil {
		return nil, err
	}
	format, err := resolveFormat(req.GetOptions())
	if err != nil {
		return nil, err
	}

	tool := restoreTool
	if format == formatPlain {
		tool = psqlTool
	}
	toolPath, err := exec.LookPath(tool)
	if err != nil {
		return nil, sdk.ToolNotFound(tool)
	}

	if err := emit(&fwv1.RestoreProgress{
		RestoreId: req.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
		Message:   "downloading the artifact",
	}); err != nil {
		return nil, err
	}

	// The artifact is spilled to a private temporary file rather than piped straight into the
	// restore tool, because its checksum has to be confirmed before a single statement is applied.
	// Restoring a corrupted artifact and only then noticing the counts are wrong wastes minutes and
	// reports the wrong cause: "the data does not match" instead of "the bytes are not the bytes we
	// wrote".
	path, size, cleanup, err := downloadArtifact(ctx, artifact, func(read int64) error {
		return emit(&fwv1.RestoreProgress{
			RestoreId: req.GetRestoreId(),
			Phase:     fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
			BytesRead: read,
			Message:   "downloading the artifact",
		})
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}

	database := strings.TrimSpace(creds.GetDatabase())
	if database == "" {
		return nil, sdk.InvalidArgument("the restore target has no database to restore into")
	}

	// The target is confirmed to answer before the restore tool is started, and after the checksum
	// has been confirmed so that a provably bad artifact is still reported as one.
	//
	// Without this, a sandbox that died between becoming ready and being restored into produces a
	// refused connection on pg_restore's stderr, in exactly the same shape as a genuine data
	// failure — and core reads a tool failure as evidence about the artifact (ADR-0022). The
	// product's one differentiating alert would then fire on a container that lost a race.
	if err := probeRestoreTarget(ctx, creds); err != nil {
		return nil, err
	}

	if err := emit(&fwv1.RestoreProgress{
		RestoreId: req.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_RUNNING,
		BytesRead: size,
		Message:   fmt.Sprintf("restoring %d bytes into %s with %s", size, database, tool),
	}); err != nil {
		return nil, err
	}

	ignored, err := runRestoreTool(ctx, restoreOptions{
		tool:     tool,
		path:     toolPath,
		creds:    creds,
		database: database,
		format:   format,
		file:     path,
	})
	if err != nil {
		return nil, err
	}

	metadata := map[string]string{
		metadataFormat:   format,
		"restored_with":  tool,
		"artifact_bytes": strconv.FormatInt(size, 10),
	}
	if source := strings.TrimSpace(req.GetOptions()[metadataDatabase]); source != "" {
		// The database the artifact was taken from, which is not the sandbox database it was just
		// loaded into. Recorded so a verification report can name what an operator recognizes.
		metadata["source_database"] = source
	}
	if ignored != "" {
		// Surfaced rather than swallowed: these are the diagnostics deliberately judged cosmetic,
		// and an operator reading a verification report should be able to see what was waved
		// through. The record counts, not this, are what decides whether the restore was good.
		metadata["ignored_diagnostics"] = ignored
	}

	// engine_version is deliberately absent: the source's version is what core already recorded on
	// the backup, and the sandbox's version is core's own choice of image. Reporting either here
	// would only invite the two to disagree.
	return &fwv1.RestoreResult{
		Duration:          durationpb.New(time.Since(started)),
		RestoredDatabases: []string{database},
		Metadata:          metadata,
	}, nil
}

// restoreTargetCredentials validates the target and returns the credentials to connect with.
func restoreTargetCredentials(target *fwv1.RestoreTarget) (*fwv1.Credentials, error) {
	switch target.GetKind() {
	case fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX:
	case fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_INSTANCE:
		return nil, sdk.Unsupported(
			"restoring over an existing instance is not implemented; this plugin restores into a " +
				"verification sandbox only")
	default:
		return nil, sdk.InvalidArgument("target.kind is required")
	}

	creds := target.GetCredentials()
	if creds.GetHost() == "" {
		return nil, sdk.InvalidArgument("target.credentials.host is required")
	}
	return creds, nil
}

// probeRestoreTarget opens and closes one connection to the instance about to be restored into.
//
// It returns whatever connect classified the failure as — ERROR_CODE_CONNECTION_FAILED or
// ERROR_CODE_AUTHENTICATION_FAILED — never a tool failure, which is the whole point: core treats a
// tool failure as evidence that the artifact is bad, and a target that does not answer is evidence
// about nothing but the target.
func probeRestoreTarget(ctx context.Context, creds *fwv1.Credentials) error {
	conn, err := connect(ctx, creds)
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

// selectArtifact picks the single artifact this method restores from.
//
// A logical dump is one self-contained file: there is no incremental chain to apply and no log to
// replay. Accepting several and quietly using the first would restore less than was asked for,
// which is the failure this product exists to detect rather than commit.
func selectArtifact(artifacts []*fwv1.ArtifactSource) (*fwv1.ArtifactSource, error) {
	var bases []*fwv1.ArtifactSource
	for _, a := range artifacts {
		switch a.GetRole() {
		case fwv1.ArtifactRole_ARTIFACT_ROLE_UNSPECIFIED, fwv1.ArtifactRole_ARTIFACT_ROLE_BASE:
			bases = append(bases, a)
		default:
			return nil, sdk.InvalidArgument(
				"the %s method restores from a single base artifact; %s was supplied",
				MethodPgDump, a.GetRole())
		}
	}

	switch len(bases) {
	case 0:
		return nil, sdk.InvalidArgument("no artifact to restore from")
	case 1:
		if bases[0].GetDownloadUrl().GetUrl() == "" {
			return nil, sdk.InvalidArgument("the artifact carries no download grant")
		}
		return bases[0], nil
	default:
		return nil, sdk.InvalidArgument(
			"the %s method restores from a single artifact; %d were supplied", MethodPgDump, len(bases))
	}
}

// downloadArtifact fetches the artifact to a private temporary file and confirms its checksum.
//
// The returned cleanup removes the file and is safe to call even when the download failed, which is
// why it is returned alongside the error rather than only on success: an artifact is a full copy of
// a database, and one left in a temporary directory outlives the verification that needed it.
func downloadArtifact(ctx context.Context, artifact *fwv1.ArtifactSource, onProgress func(int64) error) (path string, size int64, cleanup func(), err error) {
	grant := artifact.GetDownloadUrl()

	method := grant.GetMethod()
	if method == "" {
		method = http.MethodGet
	}

	dir, err := os.MkdirTemp("", "fleetward-restore-")
	if err != nil {
		return "", 0, nil, sdk.Internal("create a temporary directory for the artifact").WithCause(err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	// A neutral name, for the same reason core uses one: the file's format is decided by the
	// backup's metadata, and a name implying otherwise would be a second, disagreeing source.
	path = filepath.Join(dir, "artifact")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // G304: the path is built from MkdirTemp and a constant leaf, never from the request
	if err != nil {
		return "", 0, cleanup, sdk.Internal("create the artifact file").WithCause(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, method, grant.GetUrl(), nil)
	if err != nil {
		// The URL is a bearer credential for the artifact; only its redacted form may be named.
		return "", 0, cleanup, sdk.ObjectStoreFailed("build the download request for %s",
			sdk.SafeURL(grant.GetUrl())).WithCause(err)
	}
	for key, value := range grant.GetHeaders() {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, cleanup, sdk.ObjectStoreFailed("download the artifact from %s",
			sdk.SafeURL(grant.GetUrl())).WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", 0, cleanup, sdk.ObjectStoreFailed("object store rejected the download with %s: %s",
			resp.Status, trimStoreError(string(body)))
	}

	hasher := sha256.New()
	size, err = copyWithProgress(file, io.TeeReader(resp.Body, hasher), onProgress)
	if err != nil {
		return "", 0, cleanup, sdk.ObjectStoreFailed("read the artifact").WithCause(err)
	}
	if err := file.Close(); err != nil {
		return "", 0, cleanup, sdk.Internal("write the artifact to disk").WithCause(err)
	}
	closed = true

	if err := verifyChecksum(artifact.GetChecksum(), hasher.Sum(nil), size); err != nil {
		return "", 0, cleanup, err
	}
	if declared := artifact.GetSizeBytes(); declared > 0 && declared != size {
		return "", 0, cleanup, sdk.ArtifactCorrupt(
			"the artifact is %d bytes but %d were recorded when it was written", size, declared)
	}

	return path, size, cleanup, nil
}

// copyWithProgress streams src into dst, reporting the running total at part-sized intervals.
func copyWithProgress(dst io.Writer, src io.Reader, onProgress func(int64) error) (int64, error) {
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

// verifyChecksum compares what arrived against what was recorded when the artifact was written.
//
// This is the check that separates "the backup does not restore" from "the backup is not the bytes
// we stored". A missing checksum is refused rather than skipped: verification whose evidence chain
// has a hole in it reports a confidence it has not earned.
func verifyChecksum(expected *fwv1.Checksum, actual []byte, size int64) error {
	if expected == nil || expected.GetValue() == "" {
		return sdk.InvalidArgument(
			"the artifact carries no checksum, so it cannot be confirmed to be the one that was written")
	}
	switch expected.GetAlgorithm() {
	case fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED,
		fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256:
	default:
		return sdk.InvalidArgument("checksum algorithm %s is not implemented; this plugin computes SHA-256",
			expected.GetAlgorithm())
	}

	got := hex.EncodeToString(actual)
	if !strings.EqualFold(got, strings.TrimSpace(expected.GetValue())) {
		return sdk.ArtifactCorrupt(
			"the artifact does not match its checksum: %d bytes hash to %s, but %s was recorded when "+
				"it was written", size, got, expected.GetValue())
	}
	return nil
}

// restoreOptions is everything needed to build and run the restore child process.
type restoreOptions struct {
	tool     string
	path     string
	creds    *fwv1.Credentials
	database string
	format   string
	file     string
}

// runRestoreTool applies the artifact and returns the diagnostics it deliberately ignored.
//
// pg_restore exits non-zero for reasons that have nothing to do with the data: a role the dump
// refers to that does not exist in the sandbox, an extension comment only a superuser may set, an
// object the template database already provides. Treating every non-zero exit as a failed
// verification would report failure on every healthy restore, and an alert that fires every night
// is an alert nobody reads.
//
// So the cosmetic ones are named and waived, anything else is fatal, and the honest arbiter of
// whether the restore worked is the record count comparison that follows it. That ordering is
// deliberate: this function is allowed to be lenient precisely because something stricter runs next.
func runRestoreTool(ctx context.Context, opts restoreOptions) (string, error) {
	tlsDir, tlsFiles, err := writeTLSMaterial(opts.creds.GetTls())
	if err != nil {
		return "", err
	}
	if tlsDir != "" {
		defer func() { _ = os.RemoveAll(tlsDir) }()
	}

	// The same rule as the dump's environment: start from the parent's minus every PG* variable, so
	// a stray PGDATABASE in the control plane's environment cannot redirect a restore.
	env, err := dumpEnv(os.Environ(), opts.creds, tlsFiles)
	if err != nil {
		return "", err
	}

	args := restoreArgs(opts)
	cmd := exec.CommandContext(ctx, opts.path, args...) //nolint:gosec // G204: the path comes from LookPath and every argument is built here
	cmd.Env = env
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr
	// psql echoes nothing useful and pg_restore writes only to stderr; discarding stdout keeps a
	// plain-format restore from filling the plugin's own output.
	cmd.Stdout = io.Discard

	runErr := cmd.Run()
	fatal, cosmetic, unreachable := classifyRestoreDiagnostics(stderr.String())

	if len(unreachable) > 0 {
		// The connection was lost part way through, so everything else on stderr is the wreckage
		// rather than the cause. Reported as a connection failure and never as a tool failure: the
		// artifact was never given the chance to be wrong.
		return "", sdk.ConnectionFailed("the restore target stopped answering: %s",
			strings.Join(lastN(unreachable, 3), "; ")).WithCause(runErr)
	}
	if runErr != nil && len(fatal) == 0 && len(cosmetic) == 0 {
		// A non-zero exit with nothing on stderr means the tool itself failed to run, not that the
		// artifact was bad.
		return "", restoreFailure(opts.tool, runErr, stderr.String())
	}
	if len(fatal) > 0 {
		return "", sdk.ToolFailed(opts.tool, "%s could not restore the artifact: %s",
			opts.tool, strings.Join(lastN(fatal, 5), "; "))
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return "", restoreFailure(opts.tool, runErr, stderr.String())
		}
	}

	return strings.Join(lastN(cosmetic, 5), "; "), nil
}

// restoreArgs builds the command line for whichever tool the artifact's format calls for.
//
// The password is absent for the same reason it is absent from pg_dump's: everything on argv is
// visible through ps to every user on the host.
func restoreArgs(opts restoreOptions) []string {
	common := []string{
		"--host=" + opts.creds.GetHost(),
		"--port=" + strconv.Itoa(int(portOrDefault(opts.creds.GetPort()))),
		"--username=" + opts.creds.GetUsername(),
		"--dbname=" + opts.database,
		"--no-password",
	}

	if opts.tool == psqlTool {
		return append(common,
			// A plain dump is a script; stopping on the first error would abort on the first
			// ownership statement naming a role the sandbox has never heard of.
			"--set=ON_ERROR_STOP=0",
			"--quiet",
			"--file="+opts.file,
		)
	}

	return append(common,
		// Ownership and privileges refer to roles that exist on the source cluster and nowhere
		// else. Restoring them would fail on every object for a reason that says nothing about
		// whether the data survived; skipping them is what makes a restore into a throwaway
		// sandbox meaningful.
		"--no-owner",
		"--no-privileges",
		"--no-comments",
		opts.file,
	)
}

// cosmeticDiagnostics are the failures a restore into an empty sandbox produces for reasons that
// have nothing to do with the artifact.
//
// The list is deliberately short and specific. A pattern that matched too broadly would waive a
// real failure, and a verification that waives real failures is worse than no verification: it
// reports confidence nobody has earned.
var cosmeticDiagnostics = []string{
	"role \"",
	"already exists",
	"must be owner of",
	"must be superuser",
	"permission denied to",
	"no privileges were granted",
	"no privileges could be revoked",
	"extension \"plpgsql\"",
	// A client newer than the sandbox sets parameters the server has never heard of — pg_restore 17
	// opens every restore with `SET transaction_timeout = 0`, which PostgreSQL 16 rejects outright.
	// This is not hypothetical: the plugin's own runtime image ships the newest client it can, so
	// that it can dump the newest server in the estate, and a sandbox is deliberately pinned to the
	// older version that produced the artifact. Every one of these settings is a client-side safety
	// default whose absence means the server simply lacks the feature, so failing a restore over one
	// would mean no backup taken from an older server could ever be verified.
	"unrecognized configuration parameter",
}

// unreachableDiagnostics are the failures that mean the restore never reached a database.
//
// They are separated from the fatal ones because of what core does with each: a tool failure is
// read as evidence that the artifact could not be loaded, and reported as a failed verification
// (ADR-0022). A sandbox that went away says nothing about the artifact, and reporting it as data
// loss is how the one alert this product exists to raise gets muted.
var unreachableDiagnostics = []string{
	"connection to server",
	"could not connect to server",
	"server closed the connection unexpectedly",
	"connection refused",
	"no pg_hba.conf entry",
	"password authentication failed",
	"terminating connection due to administrator command",
}

// classifyRestoreDiagnostics splits a tool's stderr into the failures that condemn the artifact,
// the ones that condemn nothing, and the ones that mean the target was never reached.
func classifyRestoreDiagnostics(stderr string) (fatal, cosmetic, unreachable []string) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lower := strings.ToLower(line)
		// pg_restore prefixes its own failures with "error:"; psql reports the server's as "ERROR:".
		if !strings.Contains(lower, "error:") {
			continue
		}
		// pg_restore's closing tally repeats what the individual lines already said.
		if strings.Contains(lower, "errors ignored on restore") {
			continue
		}

		switch {
		case matchesDiagnostic(lower, unreachableDiagnostics):
			unreachable = append(unreachable, line)
		case matchesDiagnostic(lower, cosmeticDiagnostics):
			cosmetic = append(cosmetic, line)
		default:
			fatal = append(fatal, line)
		}
	}
	return fatal, cosmetic, unreachable
}

func matchesDiagnostic(lowered string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(lowered, pattern) {
			return true
		}
	}
	return false
}

// restoreFailure turns a tool failure with no usable diagnostics into a typed error.
func restoreFailure(tool string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = err.Error()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return sdk.ToolFailed(tool, "%s exited with status %d: %s", tool, exitErr.ExitCode(), lastLines(detail, 5)).
			WithDetail("exit_code", strconv.Itoa(exitErr.ExitCode())).
			WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return sdk.NewError(fwv1.ErrorCode_ERROR_CODE_TIMEOUT, "%s did not finish within the restore timeout", tool).
			Retry().WithCause(err)
	}
	return sdk.ToolFailed(tool, "%s failed: %s", tool, lastLines(detail, 5)).WithCause(err)
}

// lastN returns at most the last n entries, which is where a tool's real complaint lives.
func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
