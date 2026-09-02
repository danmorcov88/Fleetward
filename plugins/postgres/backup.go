package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// MethodPgDump is the identifier of the logical backup method, referenced by
	// BackupRequest.method_id.
	MethodPgDump = "pg_dump"

	// dumpTool is the executable this method orchestrates. Fleetward does not implement backup
	// formats; it drives the engine's own tooling (CLAUDE.md §4).
	dumpTool = "pg_dump"

	// optionFormat selects pg_dump's output format.
	optionFormat = "format"
	// formatCustom is pg_dump's compressed archive. It is the default because pg_restore can load
	// it into any empty database, selectively and in parallel, which is what slice A5 restores into
	// a sandbox.
	formatCustom = "custom"
	// formatPlain is a SQL script, restorable with psql alone.
	formatPlain = "plain"

	// stderrLimit bounds how much of pg_dump's diagnostic output is retained for the failure
	// message. The useful part is always the last few lines.
	stderrLimit = 8 << 10

	// defaultBackupTimeout bounds a run when the request carries no timeout of its own.
	defaultBackupTimeout = 6 * time.Hour
)

// backupMethods is the method matrix this plugin declares. It is a function rather than a variable
// so that no caller can mutate the capabilities another caller will read.
func backupMethods() []*fwv1.BackupMethod {
	return []*fwv1.BackupMethod{{
		Id:          MethodPgDump,
		DisplayName: "pg_dump (logical)",
		Description: "Logical dump of one database, taken from an exported snapshot so the " +
			"artifact and its manifest describe the same consistent point. Restores into any " +
			"empty database of a compatible version.",
		Kind:      fwv1.BackupKind_BACKUP_KIND_LOGICAL,
		IsDefault: true,
		// A logical dump is not a PITR baseline: it has no WAL position to replay from. The
		// physical method that will provide one arrives with pg_basebackup in phase B.
		EnablesPitr: false,
		// The custom format compresses by default; pg_dump takes no lock that blocks writers.
		SupportsCompression: true,
		RequiresDowntime:    false,
		RequiredTools:       []string{dumpTool},
		Options: []*fwv1.MethodOption{{
			Name:         optionFormat,
			DisplayName:  "Output format",
			Description:  "custom is a compressed archive restored with pg_restore; plain is a SQL script restored with psql.",
			Type:         fwv1.OptionType_OPTION_TYPE_ENUM,
			DefaultValue: formatCustom,
			AllowedValues: []string{
				formatCustom,
				formatPlain,
			},
		}},
	}}
}

// Backup runs a backup and streams progress.
//
// The final emitted message is always terminal — JOB_PHASE_COMPLETED with a BackupResult, or
// JOB_PHASE_FAILED with a PluginError. Returning without one would leave core unable to tell a
// success from a crashed stream, which is the difference between a backup it believes it has and
// one it actually has.
func (p *Plugin) Backup(ctx context.Context, req *fwv1.BackupRequest, emit sdk.Emitter[*fwv1.BackupProgress]) error {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultBackupTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := runBackup(runCtx, req, emit)
	if err != nil {
		// The failure travels as a terminal progress message rather than as an RPC error, so core
		// receives the structured code on the same stream it was already reading.
		return emit(&fwv1.BackupProgress{
			BackupId: req.GetBackupId(),
			Phase:    fwv1.JobPhase_JOB_PHASE_FAILED,
			Error:    sdk.AsPluginError(err),
		})
	}

	return emit(&fwv1.BackupProgress{
		BackupId:     req.GetBackupId(),
		Phase:        fwv1.JobPhase_JOB_PHASE_COMPLETED,
		BytesWritten: result.GetSizeBytes(),
		Message:      "backup complete",
		Result:       result,
	})
}

// runBackup performs the whole run and returns the result, leaving the caller to turn an error into
// the terminal failure message.
func runBackup(ctx context.Context, req *fwv1.BackupRequest, emit sdk.Emitter[*fwv1.BackupProgress]) (*fwv1.BackupResult, error) {
	started := time.Now()

	if id := req.GetMethodId(); id != "" && id != MethodPgDump {
		return nil, sdk.InvalidArgument("unknown backup method %q; this plugin implements %q", id, MethodPgDump)
	}
	format, err := resolveFormat(req.GetOptions())
	if err != nil {
		return nil, err
	}
	algorithm, err := resolveChecksumAlgorithm(req.GetTarget().GetChecksumAlgorithm())
	if err != nil {
		return nil, err
	}

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_PREPARING,
		Message:  "locating " + dumpTool,
	}); err != nil {
		return nil, err
	}

	dumpPath, err := exec.LookPath(dumpTool)
	if err != nil {
		return nil, sdk.ToolNotFound(dumpTool)
	}

	creds := req.GetCredentials()
	database, err := resolveDatabase(creds, req.GetDatabases())
	if err != nil {
		return nil, err
	}

	uploader, err := sdk.NewArtifactUploader(req.GetTarget(), func(written int64) error {
		return emit(&fwv1.BackupProgress{
			BackupId:     req.GetBackupId(),
			Phase:        fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
			BytesWritten: written,
			// percent_complete stays zero: a dump's final size is not known until it is finished,
			// and a fabricated percentage is worse than an honest byte count.
			Message: "uploading artifact",
		})
	})
	if err != nil {
		return nil, err
	}

	conn, err := connect(ctx, creds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	engineVersion, err := readServerVersion(ctx, conn)
	if err != nil {
		return nil, err
	}

	// Read-only repeatable read: the snapshot exported here is what both the manifest below and
	// pg_dump observe, which is the only way the two can be guaranteed to agree. The transaction
	// stays open for the whole run, because an exported snapshot dies with the transaction that
	// exported it.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, sdk.ConnectionFailed("open the backup transaction").WithCause(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	snapshotID, err := exportSnapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	capturedAt := time.Now()

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_PREPARING,
		Message:  "counting rows in " + database,
	}); err != nil {
		return nil, err
	}

	// The manifest is collected before the dump rather than after it, so the counting queries run
	// while the dump has not yet begun competing for I/O. Both see the same snapshot either way.
	manifest, err := collectManifest(ctx, tx, database, capturedAt)
	if err != nil {
		return nil, err
	}

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_RUNNING,
		Message:  fmt.Sprintf("dumping %s (%d tables, %d rows)", database, manifest.GetTotalObjects(), manifest.GetTotalRecords()),
	}); err != nil {
		return nil, err
	}

	parts, size, checksum, err := streamDump(ctx, dumpOptions{
		path:       dumpPath,
		creds:      creds,
		database:   database,
		format:     format,
		snapshotID: snapshotID,
	}, uploader)
	if err != nil {
		return nil, err
	}

	return &fwv1.BackupResult{
		Artifact:  req.GetTarget().GetObject(),
		SizeBytes: size,
		Checksum:  &fwv1.Checksum{Algorithm: algorithm, Value: checksum},
		Duration:  durationpb.New(time.Since(started)),
		MethodId:  MethodPgDump,
		// A logical dump restores to the point its snapshot was taken, not to when it finished.
		EngineVersion:    engineVersion,
		ConsistencyPoint: timestamppb.New(capturedAt),
		Manifest:         manifest,
		Parts:            parts,
		Metadata: map[string]string{
			// Restore needs to know which tool loads this artifact. Recording it here keeps that
			// knowledge in the plugin's own bookkeeping rather than in a core lookup table.
			"format":   format,
			"database": database,
		},
	}, nil
}

// dumpOptions is everything needed to build and run the child process.
type dumpOptions struct {
	path       string
	creds      *fwv1.Credentials
	database   string
	format     string
	snapshotID string
}

// streamDump runs pg_dump and pipes its stdout straight into the multipart upload, hashing on the
// way past.
//
// Nothing is buffered to disk and nothing larger than one part is held in memory: the artifact
// exists only as bytes in flight between the two processes. The checksum is computed from the same
// pass, because re-reading a multi-gigabyte object afterwards just to hash it would double the
// transfer for no benefit.
func streamDump(ctx context.Context, opts dumpOptions, uploader *sdk.ArtifactUploader) ([]*fwv1.UploadedPart, int64, string, error) {
	tlsDir, tlsFiles, err := writeTLSMaterial(opts.creds.GetTls())
	if err != nil {
		return nil, 0, "", err
	}
	if tlsDir != "" {
		defer func() { _ = os.RemoveAll(tlsDir) }()
	}

	env, err := dumpEnv(os.Environ(), opts.creds, tlsFiles)
	if err != nil {
		return nil, 0, "", err
	}

	cmd := exec.CommandContext(ctx, opts.path, dumpArgs(opts.creds, opts.database, opts.format, opts.snapshotID)...) //nolint:gosec // G204: the path comes from LookPath and every argument is validated above
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, "", sdk.Internal("open the %s output pipe", dumpTool).WithCause(err)
	}
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, 0, "", sdk.ToolFailed(dumpTool, "%s could not be started", dumpTool).WithCause(err)
	}

	hasher := sha256.New()
	parts, size, uploadErr := uploader.Upload(ctx, io.TeeReader(stdout, hasher))

	// pg_dump is waited on whatever happened above: an upload that failed leaves a child process
	// holding a transaction open on a production server, and only Wait reaps it. The pipe is
	// drained first so the child is never blocked writing into a reader that has gone away.
	if uploadErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()

	// The exit code is checked before the upload error is reported, because "pg_dump failed" is the
	// more useful diagnosis when both happened — a tool that dies mid-stream produces a truncated
	// artifact, and reporting the truncation instead would hide the cause.
	if waitErr != nil {
		return nil, 0, "", dumpFailure(waitErr, stderr.String())
	}
	if uploadErr != nil {
		return nil, 0, "", uploadErr
	}

	// A zero exit status with an empty archive is not a valid backup, and this is the last point at
	// which that can be caught before core writes a green row.
	if size == 0 {
		return nil, 0, "", sdk.ToolFailed(dumpTool, "%s exited successfully but produced no output", dumpTool)
	}

	return parts, size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// dumpFailure turns a non-zero exit into a typed error carrying the tail of pg_dump's diagnostics.
//
// A non-zero exit still produces output on stdout, so the parts already uploaded describe a
// truncated database. Core aborts the multipart upload on any failure, which is what keeps a
// partial artifact from ever becoming a visible object that reports success.
func dumpFailure(err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = err.Error()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return sdk.ToolFailed(dumpTool, "%s exited with status %d: %s",
			dumpTool, exitErr.ExitCode(), lastLines(detail, 5)).
			WithDetail("exit_code", strconv.Itoa(exitErr.ExitCode())).
			WithCause(err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return sdk.NewError(fwv1.ErrorCode_ERROR_CODE_TIMEOUT, "%s did not finish within the backup timeout", dumpTool).
			Retry().WithCause(err)
	}
	return sdk.ToolFailed(dumpTool, "%s failed: %s", dumpTool, lastLines(detail, 5)).WithCause(err)
}

// dumpArgs builds pg_dump's command line.
//
// The password is deliberately absent: everything on argv is visible through ps to every user on
// the host, and this process is dumping a production database. It travels in the environment
// instead, which only the child and root can read. --no-password makes a missing or wrong password
// an immediate failure rather than a prompt on a terminal nobody is watching.
func dumpArgs(creds *fwv1.Credentials, database, format, snapshotID string) []string {
	args := []string{
		"--host=" + creds.GetHost(),
		"--port=" + strconv.Itoa(int(portOrDefault(creds.GetPort()))),
		"--username=" + creds.GetUsername(),
		"--dbname=" + database,
		"--format=" + format,
		"--no-password",
	}
	if snapshotID != "" {
		// Ties the dump to the transaction that produced the manifest. Without it the two describe
		// different moments, and every count comparison at verification time becomes a coin toss.
		args = append(args, "--snapshot="+snapshotID)
	}
	return args
}

// dumpEnv builds the child's environment.
//
// It starts from the parent's, minus every PG* variable. The plugin runs as a child of the control
// plane, whose own environment may well contain PGDATABASE, PGHOST, or PGSSLMODE for unrelated
// reasons; inheriting one of those would silently redirect or downgrade a production backup.
func dumpEnv(parent []string, creds *fwv1.Credentials, tls tlsMaterial) ([]string, error) {
	env := make([]string, 0, len(parent)+8)
	for _, entry := range parent {
		if name, _, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(name, "PG") {
			continue
		}
		env = append(env, entry)
	}

	env = append(env,
		"PGPASSWORD="+creds.GetPassword(),
		// The same identifier Discover and HealthCheck use, so a DBA reading pg_stat_activity can
		// tell a Fleetward backup from anything else running on their server.
		"PGAPPNAME=fleetward",
	)

	sslMode, err := sslMode(creds.GetTls())
	if err != nil {
		return nil, err
	}
	env = append(env, "PGSSLMODE="+sslMode)
	if tls.caPath != "" {
		env = append(env, "PGSSLROOTCERT="+tls.caPath)
	}
	if tls.certPath != "" {
		env = append(env, "PGSSLCERT="+tls.certPath, "PGSSLKEY="+tls.keyPath)
	}

	return env, nil
}

// sslMode translates the contract's TLS settings into libpq's single-knob equivalent.
//
// TLS enabled with neither a CA nor an explicit decision to skip verification is refused rather
// than quietly downgraded. libpq without a root certificate cannot verify anything, and a backup
// tool that silently stops verifying the server it is dumping from is exactly the kind of quiet
// regression this project exists to catch elsewhere.
func sslMode(settings *fwv1.TLSSettings) (string, error) {
	if !settings.GetEnabled() {
		return "disable", nil
	}
	if settings.GetInsecureSkipVerify() {
		return "require", nil
	}
	if len(settings.GetCaPem()) == 0 {
		return "", sdk.InvalidArgument(
			"tls is enabled without a ca_pem: %s cannot verify the server without one. "+
				"Supply the CA certificate, or set insecure_skip_verify for a development instance.",
			dumpTool)
	}
	// verify-full also checks that the hostname matches the certificate. When the operator has
	// named a different expected server name, libpq has no equivalent knob — it always verifies
	// against the host it connected to — so verification steps down to the certificate chain alone
	// rather than failing on a mismatch the connection was configured to expect.
	if settings.GetServerName() != "" {
		return "verify-ca", nil
	}
	return "verify-full", nil
}

// tlsMaterial holds the paths PEM material was written to for the child process.
type tlsMaterial struct {
	caPath   string
	certPath string
	keyPath  string
}

// writeTLSMaterial spills the connection's PEM material to a private temporary directory, because
// libpq reads certificates from files and the contract carries them as bytes.
//
// The directory is 0700 and each file 0600, and the caller removes the whole directory when the
// dump ends.
func writeTLSMaterial(settings *fwv1.TLSSettings) (string, tlsMaterial, error) {
	if !settings.GetEnabled() {
		return "", tlsMaterial{}, nil
	}
	if len(settings.GetCaPem()) == 0 && len(settings.GetClientCertPem()) == 0 {
		return "", tlsMaterial{}, nil
	}

	// MkdirTemp creates the directory with mode 0700, which is what this material needs: the client
	// private key inside it is a credential, and one left readable on a shared host outlives the
	// backup that needed it.
	dir, err := os.MkdirTemp("", "fleetward-tls-")
	if err != nil {
		return "", tlsMaterial{}, sdk.Internal("create a temporary directory for TLS material").WithCause(err)
	}

	var material tlsMaterial
	write := func(name string, content []byte) (string, error) {
		if len(content) == 0 {
			return "", nil
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return "", sdk.Internal("write %s for the backup connection", name).WithCause(err)
		}
		return path, nil
	}

	if material.caPath, err = write("ca.pem", settings.GetCaPem()); err != nil {
		_ = os.RemoveAll(dir)
		return "", tlsMaterial{}, err
	}
	if material.certPath, err = write("client.crt", settings.GetClientCertPem()); err != nil {
		_ = os.RemoveAll(dir)
		return "", tlsMaterial{}, err
	}
	if material.keyPath, err = write("client.key", settings.GetClientKeyPem()); err != nil {
		_ = os.RemoveAll(dir)
		return "", tlsMaterial{}, err
	}
	if (material.certPath == "") != (material.keyPath == "") {
		_ = os.RemoveAll(dir)
		return "", tlsMaterial{}, sdk.InvalidArgument("tls: client certificate and key must be supplied together")
	}

	return dir, material, nil
}

// resolveFormat validates the format option.
func resolveFormat(options map[string]string) (string, error) {
	format := strings.TrimSpace(options[optionFormat])
	if format == "" {
		return formatCustom, nil
	}
	if !slices.Contains([]string{formatCustom, formatPlain}, format) {
		return "", sdk.InvalidArgument("option %q must be one of %s or %s, got %q",
			optionFormat, formatCustom, formatPlain, format)
	}
	return format, nil
}

// resolveChecksumAlgorithm accepts the algorithms this plugin can compute while streaming.
func resolveChecksumAlgorithm(requested fwv1.ChecksumAlgorithm) (fwv1.ChecksumAlgorithm, error) {
	switch requested {
	case fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED,
		fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256:
		return fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256, nil
	default:
		return 0, sdk.InvalidArgument("checksum algorithm %s is not implemented; this plugin computes SHA-256", requested)
	}
}

// resolveDatabase picks the single database this method dumps.
//
// pg_dump backs up one database at a time. An empty databases list therefore means the connection's
// own database rather than the whole cluster, and asking for several is refused rather than
// silently reduced to the first — a backup that quietly covered less than was asked for is the
// failure mode this product exists to detect.
func resolveDatabase(creds *fwv1.Credentials, requested []string) (string, error) {
	switch len(requested) {
	case 0:
		if db := strings.TrimSpace(creds.GetDatabase()); db != "" {
			return db, nil
		}
		return "", sdk.InvalidArgument(
			"no database to back up: name one in the request, or give the connection a database")
	case 1:
		if db := strings.TrimSpace(requested[0]); db != "" {
			return db, nil
		}
		return "", sdk.InvalidArgument("the requested database name is empty")
	default:
		return "", sdk.InvalidArgument(
			"the %s method backs up one database at a time; %d were requested. "+
				"Schedule one backup per database, or use a cluster-wide method when one exists.",
			MethodPgDump, len(requested))
	}
}

// readServerVersion records which server produced the artifact, so a restore can be matched to a
// compatible image.
func readServerVersion(ctx context.Context, q queryer) (string, error) {
	var raw string
	if err := q.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&raw); err != nil {
		return "", sdk.ConnectionFailed("read the server version").WithCause(err)
	}
	return normalizeVersion(raw), nil
}

// boundedBuffer keeps only the last limit bytes written to it.
//
// pg_dump can emit a very large amount of diagnostic output — one line per object with --verbose,
// or a repeated permission error — and the useful part is always the end. Retaining the tail keeps
// a failure message informative without letting a child process dictate the plugin's memory use.
type boundedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) > b.limit {
		p = p[len(p)-b.limit:]
	}
	b.buf.Write(p)
	if b.buf.Len() > b.limit {
		trimmed := b.buf.Bytes()[b.buf.Len()-b.limit:]
		next := bytes.NewBuffer(make([]byte, 0, b.limit))
		next.Write(trimmed)
		b.buf = *next
	}
	return written, nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// lastLines returns at most n trailing lines, which is where a tool's actual complaint lives.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
