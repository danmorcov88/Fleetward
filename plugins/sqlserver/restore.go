package sqlserver

import (
	"context"
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// defaultRestoreTimeout bounds a run when the request carries no timeout of its own.
	defaultRestoreTimeout = 2 * time.Hour

	// probeTimeout bounds the check that the target answers at all. Short on purpose: it runs
	// before anything expensive, and its whole job is to fail fast and correctly.
	probeTimeout = 20 * time.Second

	// fallbackDataPath is where a Linux SQL Server keeps its data files. It is used only when the
	// target refuses to say where its own default is, which a healthy instance never does.
	fallbackDataPath = "/var/opt/mssql/data/"
)

// sandboxTemplate tells core how to stand up a throwaway SQL Server for verification.
//
// Everything engine-specific about a sandbox lives here rather than in core, which is what allows a
// new engine to arrive as a plugin with no core change (CLAUDE.md §4). Three of its fields exist
// because this engine needed them and PostgreSQL did not — the fixed username, the password policy,
// and the shared directory — and each of them is a declaration core acts on without knowing which
// engine made it.
func sandboxTemplate() *fwv1.SandboxTemplate {
	return &fwv1.SandboxTemplate{
		ImageRepository: "mcr.microsoft.com/mssql/server",
		// Deliberately no tag template. SQL Server's product version and its image tag are
		// different vocabularies — 16.0.x is published as "2022-latest" — so deriving one from the
		// other would be a mapping table in a place that must not have one. A backup restores into
		// its own version or a newer one, so the newest exercised tag is the safe fixed choice.
		DefaultTag: "2022-latest",
		//nolint:gosec // G101: these are template placeholders core fills at provisioning time,
		// which is precisely the arrangement ADR-0020 exists to guarantee — a plugin binary must
		// never carry a credential.
		Env: map[string]string{
			"ACCEPT_EULA":       "Y",
			"MSSQL_PID":         "Developer",
			"MSSQL_SA_PASSWORD": "{{ .Password }}",
			// The image creates this database after the server starts, which is why the readiness
			// probe below connects to it rather than to master.
			"MSSQL_DB": "{{ .Database }}",
		},
		// The image creates exactly one account and cannot be told to rename it. A generated
		// username would produce a sandbox nobody can log in to; worse, the account the image can
		// create through MSSQL_USER holds CONTROL on one database and no server-level role, and SQL
		// Server documents that db_owner does not carry RESTORE permission. So the sandbox's
		// administrator is sa, behind a password that exists for minutes on loopback — the same
		// trust boundary PostgreSQL already has, where the generated user is the cluster superuser.
		FixedUsername: "sa",
		// Measured, not assumed: this image exits 255 with "Password validation failed" unless the
		// password carries three of the four character classes. Core's default generator misses two
		// of them about once in eight hundred, which is the worst frequency a defect can have.
		PasswordPolicy: &fwv1.PasswordPolicy{
			MinLength:           32,
			MinCharacterClasses: 3,
		},
		ContainerPort: 1433,
		// BACKUP and RESTORE read and write files on the server's own filesystem, so a sandbox
		// needs a directory the plugin can reach too (ADR-0026).
		SharedDirectory: "/var/opt/mssql/fleetward",
		// sqlcmd ships in the image but is not on PATH, so the absolute path is required. -C trusts
		// the self-signed certificate the instance generates for itself, and -d is what makes this
		// probe wait for the database rather than only for the server.
		ReadinessCommand: []string{
			"/opt/mssql-tools18/bin/sqlcmd",
			"-C",
			"-S", "127.0.0.1,{{ .Port }}",
			"-U", "{{ .Username }}",
			"-P", "{{ .Password }}",
			"-d", "{{ .Database }}",
			"-Q", "SELECT 1",
		},
		// Generous, because a first start copies the system databases into place and then runs the
		// image's own setup pass, which polls on a five-second loop.
		ReadinessTimeout: durationpb.New(4 * time.Minute),
	}
}

// Restore loads one artifact into a target instance and streams progress.
//
// Only a sandbox target is accepted in this slice. Restoring over a real instance is destructive and
// needs core's authorization and typed confirmation before a plugin should be willing to do it;
// refusing here means the capability matrix and the behaviour cannot drift apart in the meantime.
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

	if id := req.GetMethodId(); id != "" && id != MethodFull {
		return nil, sdk.InvalidArgument("unknown restore method %q; this plugin implements %q", id, MethodFull)
	}
	if req.GetPointInTime().IsValid() {
		return nil, sdk.Unsupported(
			"point-in-time restore needs log backups; the " + MethodFull +
				" method restores only to the artifact's own consistency point")
	}

	creds, err := restoreTargetCredentials(req.GetTarget())
	if err != nil {
		return nil, err
	}
	share, err := requireShare(creds)
	if err != nil {
		return nil, err
	}
	artifact, err := sdk.SelectSingleArtifact(req.GetArtifacts(), MethodFull)
	if err != nil {
		return nil, err
	}

	database := strings.TrimSpace(req.GetOptions()[metadataDatabase])
	if database == "" {
		database = strings.TrimSpace(creds.GetDatabase())
	}
	if database == "" {
		return nil, sdk.InvalidArgument("the restore target has no database to restore into")
	}

	if err := emit(&fwv1.RestoreProgress{
		RestoreId: req.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
		Message:   "downloading the artifact",
	}); err != nil {
		return nil, err
	}

	// The artifact is written down and hashed in full before a single statement runs against the
	// target. Restoring a corrupted artifact and only then noticing the counts are wrong wastes
	// minutes and reports the wrong cause: "the data does not match" instead of "the bytes are not
	// the bytes we wrote" (ADR-0022).
	enginePath, size, removeFile, err := fetchArtifactToShare(ctx, req, artifact, share)
	if removeFile != nil {
		defer removeFile()
	}
	if err != nil {
		return nil, err
	}

	// The target is confirmed to answer after the checksum has been confirmed, so that a provably
	// bad artifact is still reported as one, and before RESTORE is attempted, so that a sandbox
	// which died between becoming ready and being restored into is never mistaken for data loss.
	// Core reads a failure during a restore as evidence about the artifact; a container that lost a
	// race would otherwise fire the one alert this product cannot afford to cry wolf on.
	db, err := open(creds, masterDatabase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	if err := probeTarget(ctx, db); err != nil {
		return nil, err
	}

	if err := emit(&fwv1.RestoreProgress{
		RestoreId: req.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_RUNNING,
		Message:   "restoring " + database,
	}); err != nil {
		return nil, err
	}

	files, err := readFileList(ctx, db, enginePath)
	if err != nil {
		return nil, err
	}
	dataPath := defaultDataPath(ctx, db)

	if err := runRestoreStatement(ctx, db, database, enginePath, dataPath, files); err != nil {
		return nil, err
	}

	return &fwv1.RestoreResult{
		Duration:          durationpb.New(time.Since(started)),
		RestoredDatabases: []string{database},
		EngineVersion:     engineVersion(ctx, db),
		Metadata: map[string]string{
			metadataDatabase: database,
			"artifact_bytes": strconv.FormatInt(size, 10),
		},
	}, nil
}

// restoreTargetCredentials accepts only a sandbox, and only one with credentials.
func restoreTargetCredentials(target *fwv1.RestoreTarget) (*fwv1.Credentials, error) {
	switch target.GetKind() {
	case fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX:
	case fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_UNSPECIFIED:
		return nil, sdk.InvalidArgument("the restore target does not say what kind of target it is")
	default:
		return nil, sdk.Unsupported(
			"this plugin restores only into a sandbox; restoring over an existing instance is " +
				"destructive and is not implemented yet")
	}
	if target.GetCredentials() == nil {
		return nil, sdk.InvalidArgument("the restore target carries no credentials")
	}
	return target.GetCredentials(), nil
}

// probeTarget confirms the instance answers before anything is applied to it.
func probeTarget(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return classifyConnError(err)
	}
	return nil
}

// fetchArtifactToShare downloads the artifact into the shared directory, hashing on the way.
//
// It lands in the shared directory rather than in a temporary one because that is the only place
// the engine can read it from — the same directory, in the other direction, that the backup used.
func fetchArtifactToShare(
	ctx context.Context,
	req *fwv1.RestoreRequest,
	artifact *fwv1.ArtifactSource,
	share *fwv1.SharedDirectory,
) (enginePath string, size int64, remove func(), err error) {
	name := "fleetward-restore-" + req.GetRestoreId() + ".bak"
	local := filepath.Join(share.GetLocalPath(), name)
	// Joined with a forward slash: this path is interpreted by the database server, which is not
	// necessarily running the same operating system as the plugin.
	enginePath = path.Join(share.GetEnginePath(), name)

	file, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_EXCL, artifactMode) //nolint:gosec // G304: built from the configured directory and a core-assigned identifier
	if err != nil {
		return "", 0, nil, sdk.Internal(
			"create the artifact file in the shared directory; check that %s exists and is writable",
			share.GetLocalPath()).WithCause(err)
	}
	remove = func() { _ = os.Remove(local) }

	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	// The engine reads this file as its own user, so it has to be readable by more than the plugin.
	if err := os.Chmod(local, artifactMode); err != nil {
		return "", 0, remove, sdk.Internal("open the artifact file to the engine").WithCause(err)
	}

	size, err = sdk.FetchArtifact(ctx, artifact, file, nil)
	if err != nil {
		return "", size, remove, err
	}
	if err := file.Close(); err != nil {
		return "", size, remove, sdk.Internal("write the artifact to the shared directory").WithCause(err)
	}
	closed = true

	return enginePath, size, remove, nil
}

// backupFile is one file inside a backup set, as RESTORE FILELISTONLY reports it.
type backupFile struct {
	logicalName  string
	physicalName string
	fileType     string
}

// readFileList asks the engine what is inside the backup set.
//
// It is required rather than optional: the artifact's files were written under the source's paths,
// which are not the target's, so every one of them has to be relocated by name. It is also the first
// statement that reads the artifact, which makes it the first place a damaged one is caught.
func readFileList(ctx context.Context, db *sql.DB, enginePath string) ([]backupFile, error) {
	rows, err := db.QueryContext(ctx, "RESTORE FILELISTONLY FROM DISK = "+quoteLiteral(enginePath))
	if err != nil {
		return nil, restoreFailure(err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return nil, restoreFailure(err)
	}
	// FILELISTONLY's result set has gained columns in most releases and will gain more, so it is
	// read by name into a slice sized at runtime rather than by position into a fixed struct.
	values := make([]any, len(columns))
	scan := make([]any, len(columns))
	for i := range values {
		scan[i] = &values[i]
	}

	var files []backupFile
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return nil, sdk.Internal("read the artifact's file list").WithCause(err)
		}
		var f backupFile
		for i, name := range columns {
			switch strings.ToLower(name) {
			case "logicalname":
				f.logicalName = asString(values[i])
			case "physicalname":
				f.physicalName = asString(values[i])
			case "type":
				f.fileType = asString(values[i])
			}
		}
		if f.logicalName != "" {
			files = append(files, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, restoreFailure(err)
	}
	if len(files) == 0 {
		return nil, sdk.ArtifactCorrupt("the artifact contains no database files")
	}
	return files, nil
}

// asString renders one driver value as text, whatever the driver chose to decode it as.
func asString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// defaultDataPath asks the target where it keeps its data files, falling back to the Linux default.
func defaultDataPath(ctx context.Context, db *sql.DB) string {
	var configured sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512))`).Scan(&configured)
	if err == nil && configured.Valid && configured.String != "" {
		return configured.String
	}
	return fallbackDataPath
}

// runRestoreStatement applies the artifact.
//
// Every file is relocated with MOVE, because the paths recorded in the backup set are the source
// server's and the target's filesystem is its own. REPLACE is what allows the restore to overwrite
// the empty database the sandbox image created at startup. CHECKSUM makes the engine re-validate
// every page it reads, which is what turns bit rot into a diagnosis rather than into a database
// that restores and is quietly wrong.
func runRestoreStatement(ctx context.Context, db *sql.DB, database, enginePath, dataPath string, files []backupFile) error {
	var moves []string
	for _, f := range files {
		suffix := ".mdf"
		switch strings.ToUpper(f.fileType) {
		case "L":
			suffix = ".ldf"
		case "D":
			if strings.HasSuffix(strings.ToLower(f.physicalName), ".ndf") {
				suffix = ".ndf"
			}
		default:
			// FILESTREAM and full-text containers are directories rather than files, and relocating
			// them needs decisions this slice does not make.
			return sdk.Unsupported(
				"the artifact contains a %q file, which this plugin cannot relocate", f.fileType)
		}
		target := joinEnginePath(dataPath, sanitizeFileName(database+"_"+f.logicalName)+suffix)
		moves = append(moves, "MOVE "+quoteLiteral(f.logicalName)+" TO "+quoteLiteral(target))
	}

	stmt := "RESTORE DATABASE " + quoteIdentifier(database) +
		" FROM DISK = " + quoteLiteral(enginePath) +
		" WITH " + strings.Join(moves, ", ") +
		", REPLACE, CHECKSUM, RECOVERY"

	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return restoreFailure(err)
	}
	return nil
}

// joinEnginePath joins a directory and a leaf using the separator the directory already uses, since
// the target may be running Windows while the plugin is not, or the other way round.
func joinEnginePath(dir, leaf string) string {
	if strings.Contains(dir, "\\") {
		return strings.TrimRight(dir, "\\") + "\\" + leaf
	}
	return strings.TrimRight(dir, "/") + "/" + leaf
}

// sanitizeFileName reduces a logical file name to something legal on either platform's filesystem.
func sanitizeFileName(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, name)
}

// Numbered messages SQL Server uses to say that a backup set cannot be read.
//
// These are the ones that are evidence about the artifact, and nothing else on the restore path is.
// The list is short and specific on purpose: a pattern that matched too widely would blame the
// artifact for a broken sandbox, which is the failure ADR-0022 exists to prevent, and every entry
// here was observed against a real corrupted artifact or is documented as a backup-set diagnosis.
var artifactDamageNumbers = []int32{
	3203,  // Read on "…" failed — a truncated or short artifact
	3241,  // The media family on device "…" is incorrectly formed
	3242,  // The file on device "…" is not a valid media set
	3243,  // The media family was generated by a different version
	3183,  // RESTORE detected an error on page … / the database was damaged
	11801, // RESTORE detected one or more corrupted pages in the backup set
	3266,  // The backup data is incorrectly formatted
}

// restoreFailure classifies a failed RESTORE.
//
// The whole of ADR-0022 lives in this function. FAILED is reserved for evidence about the artifact,
// and everything else — a target that stopped answering, a path the engine cannot open, a
// permission it does not hold — is something the operator should look at rather than an alert
// saying their backup is bad.
func restoreFailure(err error) error {
	var engineErr mssqlError
	if !asEngineError(err, &engineErr) {
		// No numbered message at all means the conversation with the server ended rather than the
		// server answering. That is the machinery, not the artifact.
		return sdk.ConnectionFailed("the restore target stopped answering during the restore").WithCause(err)
	}

	if engineErr.hasNumber(artifactDamageNumbers...) {
		return sdk.ArtifactCorrupt("the engine refused the backup set: %s", engineMessage(err)).WithCause(err)
	}
	if engineErr.hasNumber(3201) {
		return sdk.InvalidArgument(
			"the engine could not open the artifact where the shared directory says it is: %s",
			engineMessage(err)).WithCause(err)
	}
	if engineErr.hasNumber(3101, 3159, 3154) {
		// In use by another session, the log tail was not backed up, or a database of that name
		// already exists from a different backup set. All three are about the target's state.
		return sdk.Internal("the restore target refused the restore: %s", engineMessage(err)).WithCause(err)
	}
	return sdk.Internal("the restore failed: %s", engineMessage(err)).WithCause(err)
}
