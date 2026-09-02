package sqlserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// MethodFull is the identifier of the native full-database backup, referenced by
	// BackupRequest.method_id.
	MethodFull = "full"

	// metadataDatabase is the key this method writes into BackupResult.metadata. Core stores that
	// map verbatim and hands it back as RestoreRequest.options, which is how a restore learns what
	// the artifact is without core ever learning an engine's file formats.
	metadataDatabase = "database"

	// defaultBackupTimeout bounds a run when the request carries no timeout of its own.
	defaultBackupTimeout = 6 * time.Hour

	// artifactMode lets the engine, which runs as its own user, write the file the plugin created.
	//
	// The plugin creating it is the whole trick. SQL Server on Linux writes a backup file 0640 and
	// owned by itself, and ignores umask — measured, not assumed — so a file it creates is one the
	// plugin cannot read back. Opening an existing file preserves that file's owner and mode, so
	// the artifact stays the plugin's. See ADR-0026.
	artifactMode = 0o666
)

// backupMethods is the method matrix this plugin declares. It is a function rather than a variable
// so that no caller can mutate the capabilities another caller will read.
func backupMethods() []*fwv1.BackupMethod {
	return []*fwv1.BackupMethod{{
		Id:          MethodFull,
		DisplayName: "Full database backup",
		Description: "Native BACKUP DATABASE of one database, taken with page checksums so the " +
			"engine can refuse a damaged backup set on its own. Restores into any instance of the " +
			"same major version or newer.",
		Kind:      fwv1.BackupKind_BACKUP_KIND_PHYSICAL,
		IsDefault: true,
		// A full backup is a PITR baseline only once log backups exist to replay onto it, and this
		// slice does not take them. Declaring otherwise would be a promise discovered during a
		// recovery, which is the worst moment to discover one.
		EnablesPitr:         false,
		SupportsCompression: true,
		RequiresDowntime:    false,
		// BACKUP DATABASE writes a file on the database server's own filesystem. This is the flag
		// core reads before it will schedule anything against an instance (ADR-0026).
		RequiresSharedDirectory: true,
		// Nothing is shelled out to: BACKUP is a statement, not a tool.
		RequiredTools: nil,
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

	if id := req.GetMethodId(); id != "" && id != MethodFull {
		return nil, sdk.InvalidArgument("unknown backup method %q; this plugin implements %q", id, MethodFull)
	}

	creds := req.GetCredentials()
	share, err := requireShare(creds)
	if err != nil {
		return nil, err
	}
	database, err := resolveDatabase(creds, req.GetDatabases())
	if err != nil {
		return nil, err
	}

	uploader, err := sdk.NewArtifactUploader(req.GetTarget(), func(written int64) error {
		return emit(&fwv1.BackupProgress{
			BackupId:     req.GetBackupId(),
			Phase:        fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
			BytesWritten: written,
			Message:      "uploading the artifact",
		})
	})
	if err != nil {
		return nil, err
	}

	db, err := open(creds, masterDatabase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	if err := ping(ctx, db); err != nil {
		return nil, err
	}

	// The file is created here, empty, and BACKUP writes into it rather than creating its own. That
	// is what keeps the artifact readable by the plugin afterwards (ADR-0026).
	local, engine, removeFile, err := createArtifactFile(share, req.GetBackupId())
	if err != nil {
		return nil, err
	}
	defer removeFile()

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_PREPARING,
		Message:  "reading the source's row counts",
	}); err != nil {
		return nil, err
	}

	// Taken before the backup starts, and compared with a second snapshot after the counting pass.
	// See collectManifest for why the manifest cannot simply be exact.
	sourceDB, err := open(creds, database)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceDB.Close() }()

	before, err := rowCountSnapshot(ctx, sourceDB)
	if err != nil {
		return nil, err
	}

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_RUNNING,
		Message:  "backing up " + database,
	}); err != nil {
		return nil, err
	}

	if err := runBackupStatement(ctx, db, database, engine); err != nil {
		return nil, err
	}

	manifest, err := collectManifest(ctx, sourceDB, database, before, time.Now())
	if err != nil {
		return nil, err
	}

	if err := emit(&fwv1.BackupProgress{
		BackupId: req.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
		Message:  "uploading the artifact",
	}); err != nil {
		return nil, err
	}

	file, err := os.Open(local) //nolint:gosec // G304: the path was built here from the shared directory and the backup id
	if err != nil {
		return nil, sdk.Internal("read back the backup file the engine wrote").WithCause(err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	parts, size, err := uploader.Upload(ctx, io.TeeReader(file, hasher))
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, sdk.Internal("the engine reported a successful backup but wrote no bytes")
	}

	return &fwv1.BackupResult{
		Artifact:  req.GetTarget().GetObject(),
		SizeBytes: size,
		Checksum: &fwv1.Checksum{
			Algorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
			Value:     hex.EncodeToString(hasher.Sum(nil)),
		},
		Duration:      durationpb.New(time.Since(started)),
		MethodId:      MethodFull,
		EngineVersion: engineVersion(ctx, db),
		// The artifact is consistent at the point BACKUP finished, not at the point it started.
		ConsistencyPoint: timestamppb.New(time.Now()),
		Manifest:         manifest,
		Parts:            parts,
		// Handed straight back to Restore as its options. Core stores it and never reads it.
		Metadata: map[string]string{metadataDatabase: database},
	}, nil
}

// requireShare refuses a backup that has nowhere to put its file.
//
// The message names both halves, because the usual mistake is configuring one of them: a path the
// engine writes to that the plugin cannot see produces a backup that appears to succeed and then
// cannot be read.
func requireShare(creds *fwv1.Credentials) (*fwv1.SharedDirectory, error) {
	share := creds.GetSharedDirectory()
	if share.GetEnginePath() == "" || share.GetLocalPath() == "" {
		return nil, sdk.InvalidArgument(
			"this instance has no shared directory: BACKUP DATABASE writes a file on the database " +
				"server, so Fleetward needs the path the server writes to and the path it reaches " +
				"the same directory by")
	}
	return share, nil
}

// createArtifactFile makes the empty file BACKUP will write into, and returns both of its names.
//
// The engine-side path is joined with a forward slash regardless of this host's separator: it is
// interpreted by the database server, which may not be running the same operating system as the
// plugin. The local path is joined the way this process's filesystem expects.
func createArtifactFile(share *fwv1.SharedDirectory, backupID string) (local, engine string, remove func(), err error) {
	name := "fleetward-" + backupID + ".bak"
	local = filepath.Join(share.GetLocalPath(), name)
	engine = path.Join(share.GetEnginePath(), name)

	file, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_EXCL, artifactMode) //nolint:gosec // G304: built from the configured directory and a core-assigned identifier
	if err != nil {
		return "", "", func() {}, sdk.Internal(
			"create the backup file in the shared directory; check that %s exists and is writable",
			share.GetLocalPath()).WithCause(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(local)
		return "", "", func() {}, sdk.Internal("create the backup file in the shared directory").WithCause(err)
	}
	// O_CREATE applies the process umask, which on a typical host clears the group and other write
	// bits the engine needs. The mode is therefore set explicitly rather than requested.
	if err := os.Chmod(local, artifactMode); err != nil {
		_ = os.Remove(local)
		return "", "", func() {}, sdk.Internal(
			"open the backup file to the engine; the database server writes as its own user").WithCause(err)
	}

	// Deferred on every path out of the backup, including failure. An artifact left on a share is a
	// full copy of a database that nothing will ever come back for.
	return local, engine, func() { _ = os.Remove(local) }, nil
}

// runBackupStatement asks the engine to write the artifact.
//
// FORMAT and INIT together are what make SQL Server accept the empty file the plugin created as a
// fresh media set; without FORMAT it reads the file's non-existent header and refuses. CHECKSUM
// makes it validate page checksums as it writes, which is the second, independent detector behind
// the SHA-256 core records.
func runBackupStatement(ctx context.Context, db *sql.DB, database, enginePath string) error {
	stmt := "BACKUP DATABASE " + quoteIdentifier(database) +
		" TO DISK = " + quoteLiteral(enginePath) +
		" WITH FORMAT, INIT, CHECKSUM, COMPRESSION, NAME = N'Fleetward full backup'"

	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return backupFailure(database, err)
	}
	return nil
}

// backupFailure turns a failed BACKUP into the typed error core classifies on.
func backupFailure(database string, err error) error {
	var mssqlErr mssqlError
	if asEngineError(err, &mssqlErr) {
		switch mssqlErr.number {
		case 3201:
			// Cannot open the backup device. Almost always the shared directory: the path does not
			// exist on the server, or the engine cannot write there.
			return sdk.InvalidArgument(
				"the engine could not write to the backup path; check that the shared directory's "+
					"engine path exists on the database server and is writable by it: %s",
				mssqlErr.message).WithCause(err)
		case 911, 916:
			return sdk.InvalidArgument("database %q does not exist on this instance", database).WithCause(err)
		}
	}
	return sdk.Internal("the engine refused to back up %q: %s", database, engineMessage(err)).WithCause(err)
}
