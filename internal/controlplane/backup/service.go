// Package backup orchestrates taking a backup: it creates the metadata rows, hands the plugin a
// scoped grant to write the artifact, consumes the progress stream, and persists what came back.
//
// Three rules run through it.
//
//   - Nothing here knows what an engine is. The method, the artifact's shape, and the manifest's
//     contents all come from the plugin; core supplies identity, storage, and bookkeeping.
//   - A backup row is only ever green when the artifact behind it exists and is complete. Every
//     failure path aborts the upload, so a partial artifact never becomes a visible object.
//   - The plugin holds no storage credential. It writes through presigned part grants and reports
//     receipts; core assembles the object (ADR-0007, ADR-0021).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
	"github.com/danmorcov88/fleetward/internal/controlplane/sandbox"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
)

// Sentinel errors. The gRPC layer maps them to status codes and is the only thing that decides what
// a client sees.
var (
	// ErrNotFound reports that no such row exists in this tenant.
	ErrNotFound = errors.New("not found")
	// ErrInvalidArgument reports a malformed request.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrAlreadyRunning reports that this instance already has a backup in flight.
	ErrAlreadyRunning = errors.New("a backup is already running for this instance")
	// ErrEngineUnavailable reports that no plugin currently serves the instance's engine.
	ErrEngineUnavailable = errors.New("engine unavailable")
	// ErrUnsupported reports that the plugin cannot back this instance up.
	ErrUnsupported = errors.New("unsupported")
	// ErrPluginFailed reports a failure the plugin returned. Its message is safe to show a client:
	// the contract forbids credentials in one.
	ErrPluginFailed = errors.New("plugin call failed")
)

const (
	// artifactFilename is the object's leaf name. It is deliberately neutral: the artifact's format
	// is the plugin's business, recorded in the backup's metadata, and core naming it "dump" or
	// "tar" would be core acquiring engine knowledge it must not have.
	artifactFilename = "artifact"

	// uploadParts is how many presigned part grants core issues for one backup.
	//
	// The size of a streamed artifact is unknown when the grants are minted, so a bound has to be
	// chosen in advance. At the default 64 MiB part size this covers a 64 GiB artifact; the plugin
	// fails loudly with an actionable message rather than silently truncating if it runs out.
	uploadParts = 1024

	// runTimeout bounds one backup run. A backup that has not finished in this long is not going to,
	// and leaving it running holds a repeatable-read transaction open on a production server.
	runTimeout = 6 * time.Hour

	// runGrace is how much longer core gives a run than the deadline it hands the plugin, so a
	// plugin that honours its timeout is always the one that decides the outcome.
	runGrace = 5 * time.Minute

	// progressLogInterval throttles progress logging. A plugin may emit a message per part, and a
	// large backup would otherwise write thousands of identical-looking lines.
	progressLogInterval = 30 * time.Second

	// originManaged and originObserved are the two origins a backup can have (ADR-0015). They are
	// the values of backups.origin, and the distinction is load-bearing rather than descriptive:
	// only a managed backup carries a manifest Fleetward captured, and therefore only a managed
	// backup can ever be verified.
	originManaged  = "managed"
	originObserved = "observed"

	// defaultListPageSize and maxListPageSize bound a history listing. Fifty instances backed up
	// nightly for a year is eighteen thousand rows, and something has to say no.
	defaultListPageSize = 50
	maxListPageSize     = 500
)

// Router is the slice of the plugin manager this service needs.
type Router interface {
	Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error)
}

// Resolver materializes an instance's credentials for a single plugin call. The inventory service
// implements it; depending on the interface keeps this package's tests free of a secrets provider.
type Resolver interface {
	ResolveConnection(ctx context.Context, instanceID string) (*inventory.Connection, error)
}

// Service runs backups and answers questions about them.
type Service struct {
	pool     *pgxpool.Pool
	store    objstore.ObjectStore
	plugins  Router
	resolver Resolver
	// sandboxes provisions the throwaway instances a verification restores into. It may be nil on a
	// control plane with no container runtime, which makes verification unavailable rather than
	// making the whole service unusable — a backup can still be taken and reported.
	sandboxes sandbox.Provider
	// retention is what a sweep is allowed to do. It is configuration rather than a request
	// parameter because there is one answer per control plane, and because both the sweep and the
	// preview must be reading the same one — a preview that disagreed with the sweep would be worse
	// than none, since it would be believed.
	retention RetentionPolicy
	log       *slog.Logger
	tenantID  string

	// runCtx outlives the request that started a backup: an HTTP request cannot stay open for the
	// hours a real backup takes, so RunBackup returns as soon as the rows exist and the work
	// continues here. Close cancels it, and running backups then fail rather than being abandoned
	// silently.
	runCtx    context.Context
	cancelRun context.CancelFunc
	running   sync.WaitGroup
}

// New builds the service. The tenant is fixed until OIDC lands in phase F, exactly as in inventory.
func New(pool *pgxpool.Pool, store objstore.ObjectStore, plugins Router, resolver Resolver, sandboxes sandbox.Provider, retention RetentionPolicy, log *slog.Logger) *Service {
	runCtx, cancel := context.WithCancel(context.Background())
	return &Service{
		pool:      pool,
		store:     store,
		plugins:   plugins,
		resolver:  resolver,
		sandboxes: sandboxes,
		retention: retention,
		log:       log.With(slog.String("component", "backup")),
		tenantID:  metadb.DefaultTenantID,
		runCtx:    runCtx,
		cancelRun: cancel,
	}
}

// Close cancels every running backup and verification and waits for them to record their outcome.
//
// Waiting matters: a run that is killed without writing its row leaves a backup stuck in `running`
// forever, and slice B4 — which owns leases and restart recovery — is what will make that
// recoverable. Until then, a clean shutdown is the only thing that closes the row.
func (s *Service) Close() error {
	s.cancelRun()
	s.running.Wait()
	return nil
}

// RunBackupInput describes a manually triggered backup.
type RunBackupInput struct {
	InstanceID string
	MethodID   string
	Options    map[string]string
	Databases  []string
	// VerifyOnCompletion chains a verification onto a successful backup.
	VerifyOnCompletion bool
	// TriggeredManually records that a human asked for this run rather than a schedule.
	TriggeredManually bool
	// JobID attaches this backup to a job row that already exists. The scheduler sets it, because
	// the job is what it holds the lease on, and creating a second one would collide with
	// idx_jobs_one_active_per_instance_kind. Empty means "create the job too", which is the manual
	// path.
	JobID string
	// ScheduleID records which recurring intent produced this backup. Empty for a manual run.
	ScheduleID string
	// RetentionDays is how long the artifact this run produces should be kept, snapshotted from the
	// schedule when the job was materialized. Zero stamps no expiry, and a backup with no expiry is
	// never removed by retention (ADR-0031) — which is the manual path, and every backup taken
	// before retention existed.
	RetentionDays int32
}

// prepared is everything resolved before a run starts, while the caller's context was still alive.
type prepared struct {
	client fwv1.EnginePluginClient
	conn   *inventory.Connection
	method *fwv1.BackupMethod
}

// prepare resolves the connection, the plugin, and the method, and validates the options.
//
// It is shared by the asynchronous RPC path and the synchronous scheduled one, so that a scheduled
// backup cannot be validated differently from a manual one. The two paths differ in who waits for
// the result, and in nothing else.
func (s *Service) prepare(ctx context.Context, in RunBackupInput) (prepared, error) {
	conn, err := s.resolver.ResolveConnection(ctx, in.InstanceID)
	if err != nil {
		return prepared{}, err
	}

	client, caps, err := s.plugins.Client(conn.EngineType)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, conn.EngineType, err)
	}

	method, err := selectMethod(caps, in.MethodID)
	if err != nil {
		return prepared{}, err
	}
	if err := validateOptions(method, in.Options); err != nil {
		return prepared{}, err
	}
	if err := requireSharedDirectory(method, conn); err != nil {
		return prepared{}, err
	}
	return prepared{client: client, conn: conn, method: method}, nil
}

// requireSharedDirectory refuses a method that hands its artifact over as a file when the instance
// has nowhere to hand it over.
//
// The check is here rather than in the plugin because core is what decides to start a run at all,
// and the answer must arrive when a human is asking — creating a schedule, triggering a backup —
// rather than at 02:00 from a plugin nobody is watching. Core learns nothing about the engine: the
// requirement is a flag on the method the plugin published (ADR-0026).
func requireSharedDirectory(method *fwv1.BackupMethod, conn *inventory.Connection) error {
	if !method.GetRequiresSharedDirectory() {
		return nil
	}
	share := conn.Credentials.GetSharedDirectory()
	if share.GetEnginePath() != "" && share.GetLocalPath() != "" {
		return nil
	}
	return fmt.Errorf("%w: the %q backup method writes its artifact to the database server's own "+
		"filesystem, so this instance needs a shared directory: set the connection's engine_path to "+
		"the directory the server writes to, and local_path to where this control plane reaches the "+
		"same directory", ErrInvalidArgument, method.GetId())
}

// RunBackup starts a backup and returns as soon as it has been recorded.
//
// It is asynchronous because a backup takes minutes to hours and an HTTP request must not. The
// caller polls GetBackup; the run itself is bounded by runTimeout and by the service's lifetime.
func (s *Service) RunBackup(ctx context.Context, in RunBackupInput) (backupID, jobID string, err error) {
	p, err := s.prepare(ctx, in)
	if err != nil {
		return "", "", err
	}

	// The rows are created before any work starts, so a backup that fails at the very first step is
	// still visible as a failed backup rather than as nothing at all.
	jobID, backupID, err = s.createRows(ctx, p.conn.InstanceID, p.method.GetId(), in)
	if err != nil {
		return "", "", err
	}

	s.log.InfoContext(ctx, "backup started",
		slog.String("backup_id", backupID),
		slog.String("job_id", jobID),
		slog.String("instance_id", p.conn.InstanceID),
		slog.String("engine_type", p.conn.EngineType),
		slog.String("method_id", p.method.GetId()))

	s.running.Add(1)
	go func() {
		defer s.running.Done()
		// The error is discarded because execute has already recorded and logged it, and there is
		// no caller left: RunBackup returned as soon as the rows existed.
		_ = s.execute(s.runCtx, p.client, runRequest{
			backupID:   backupID,
			jobID:      jobID,
			connection: p.conn,
			methodID:   p.method.GetId(),
			options:    in.Options,
			databases:  in.Databases,
			verify:     in.VerifyOnCompletion,

			retentionDays: in.RetentionDays,
		})
	}()

	return backupID, jobID, nil
}

// RunBackupSync runs a backup against a job that already exists, and returns once the outcome has
// been recorded.
//
// This is the scheduler's entry point. The two differences from RunBackup are both deliberate. It
// does not create a job, because the caller already holds a lease on one. And it is synchronous and
// bound to the caller's context, because a runner that loses its lease must be able to stop the
// work — a cancelled context is the only thing that reaches through the plugin to the native tool
// it is driving.
//
// It registers with the same WaitGroup, so Close waits for a scheduled run exactly as it waits for
// a manual one.
func (s *Service) RunBackupSync(ctx context.Context, in RunBackupInput) (backupID string, err error) {
	if in.JobID == "" {
		return "", fmt.Errorf("%w: a synchronous backup must name the job it belongs to", ErrInvalidArgument)
	}

	p, err := s.prepare(ctx, in)
	if err != nil {
		return "", err
	}

	_, backupID, err = s.createRows(ctx, p.conn.InstanceID, p.method.GetId(), in)
	if err != nil {
		return "", err
	}

	// job_id is not named here: the scheduler put it on the context, and telemetry promotes it.
	s.log.InfoContext(ctx, "scheduled backup started",
		slog.String("backup_id", backupID),
		slog.String("instance_id", p.conn.InstanceID),
		slog.String("engine_type", p.conn.EngineType),
		slog.String("method_id", p.method.GetId()))

	s.running.Add(1)
	defer s.running.Done()

	return backupID, s.execute(ctx, p.client, runRequest{
		backupID:   backupID,
		jobID:      in.JobID,
		connection: p.conn,
		methodID:   p.method.GetId(),
		options:    in.Options,
		databases:  in.Databases,
		// Never chained here. The scheduler queues verification as its own job row, so that the
		// policy behind it is visible in the job table and so that it competes for the same
		// concurrency budget as everything else.
		verify: false,

		retentionDays: in.RetentionDays,
	})
}

// runRequest is one backup's inputs, assembled while the caller's context was still alive.
type runRequest struct {
	backupID   string
	jobID      string
	connection *inventory.Connection
	methodID   string
	options    map[string]string
	databases  []string
	verify     bool
	// retentionDays travels with the run so the expiry is stamped from the value that was in force
	// when the backup was asked for, not from whatever the schedule says by the time it finishes.
	retentionDays int32
}

// execute performs the run and records its outcome.
//
// Every outcome is written to the database and the log regardless of what the caller does with the
// returned error: on the asynchronous path there is no caller left to receive one. The scheduler is
// the caller that does care, because whether the backup succeeded is what decides if a verification
// is queued behind it.
//
// The parent context is a parameter rather than always s.runCtx for the other half of the same
// reason. A scheduled run belongs to the runner holding its lease, and a lost lease must be able to
// stop the work — which it can only do through the context that reaches the plugin and the native
// tool it drives.
func (s *Service) execute(parent context.Context, client fwv1.EnginePluginClient, req runRequest) error {
	ctx, cancel := context.WithTimeout(parent, runTimeout+runGrace)
	defer cancel()

	log := s.log.With(slog.String("backup_id", req.backupID), slog.String("instance_id", req.connection.InstanceID))
	started := time.Now()

	result, err := s.transfer(ctx, client, req, log)
	if err != nil {
		// The recording context is detached from the run's: a run cancelled by shutdown or by the
		// timeout must still be able to write down that it failed.
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer recordCancel()

		log.ErrorContext(recordCtx, "backup failed", slog.String("error", err.Error()))
		if recordErr := s.recordFailure(recordCtx, req, err); recordErr != nil {
			log.ErrorContext(recordCtx, "could not record the backup failure", slog.String("error", recordErr.Error()))
		}
		return err
	}

	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer recordCancel()

	if err := s.recordSuccess(recordCtx, req, result); err != nil {
		// The artifact exists but the row does not describe it. That is the one outcome worse than
		// a failed backup, because the estate view would show nothing where a good artifact sits,
		// so it is logged at error level with the key needed to find the object by hand.
		log.ErrorContext(recordCtx, "backup succeeded but its result could not be recorded",
			slog.String("object_key", result.GetArtifact().GetKey()),
			slog.String("error", err.Error()))
		return err
	}

	log.InfoContext(recordCtx, "backup succeeded",
		slog.Int64("size_bytes", result.GetSizeBytes()),
		slog.Int64("manifest_objects", result.GetManifest().GetTotalObjects()),
		slog.Int64("manifest_records", result.GetManifest().GetTotalRecords()),
		slog.Duration("duration", time.Since(started)))

	if !req.verify {
		return nil
	}

	// A verification asked for with the backup starts here rather than in RunBackup, because it can
	// only begin once the artifact exists. It runs as its own job with its own rows: a backup that
	// succeeded and a verification that failed are two different facts, and the estate view has to
	// be able to show both at once.
	//
	// Failing to start one is logged rather than allowed to touch the backup's own outcome. The
	// backup is good; what is missing is the proof, and rewriting a green row to red would be a
	// lie in the opposite direction.
	if _, _, err := s.RunVerification(recordCtx, RunVerificationInput{BackupID: req.backupID}); err != nil {
		log.ErrorContext(recordCtx, "the backup succeeded but its verification could not be started",
			slog.String("error", err.Error()))
	}
	return nil
}

// transfer mints the upload grant, drives the plugin, and completes the object.
//
// The deferred abort is the guarantee the whole design rests on: on every path that does not reach
// a completed upload — a plugin failure, a stream that died, a checksum core could not persist, a
// panic — the parts already written are discarded and no object appears. A partial artifact that
// reported success would be a backup believed good and proven bad only at restore time.
func (s *Service) transfer(ctx context.Context, client fwv1.EnginePluginClient, req runRequest, log *slog.Logger) (*fwv1.BackupResult, error) {
	key := artifactKeyFor(s.tenantID, req.connection.InstanceID, req.backupID)

	upload, err := s.store.CreateMultipartUpload(ctx, key, uploadParts, 0)
	if err != nil {
		return nil, fmt.Errorf("begin the artifact upload: %w", err)
	}

	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.store.AbortMultipartUpload(abortCtx, key, upload.UploadID); err != nil {
			// Nothing is visible in the bucket either way; what leaks is storage the parts occupy
			// until a lifecycle rule reaps them, which is worth an operator's attention.
			log.WarnContext(abortCtx, "could not abort the incomplete artifact upload",
				slog.String("object_key", key), slog.String("error", err.Error()))
		}
	}()

	result, err := s.stream(ctx, client, req, upload, log)
	if err != nil {
		return nil, err
	}

	parts := make([]objstore.CompletedPart, 0, len(result.GetParts()))
	for _, part := range result.GetParts() {
		parts = append(parts, objstore.CompletedPart{PartNumber: int(part.GetPartNumber()), ETag: part.GetEtag()})
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("the plugin reported success without uploading any part of the artifact")
	}

	info, err := s.store.CompleteMultipartUpload(ctx, key, upload.UploadID, parts)
	if err != nil {
		return nil, fmt.Errorf("complete the artifact upload: %w", err)
	}
	completed = true

	// The store's own view of the object is authoritative for its size. A disagreement means the
	// plugin and the bucket describe different artifacts, and there is no safe way to guess which.
	if info.Size != result.GetSizeBytes() {
		if err := s.store.Delete(context.WithoutCancel(ctx), key); err != nil {
			log.WarnContext(ctx, "could not delete a mismatched artifact",
				slog.String("object_key", key), slog.String("error", err.Error()))
		}
		return nil, fmt.Errorf("the stored artifact is %d bytes but the plugin reported %d",
			info.Size, result.GetSizeBytes())
	}

	result.Artifact = &fwv1.ObjectRef{Bucket: s.store.Bucket(), Key: key}
	return result, nil
}

// stream drives the plugin's Backup RPC to its terminal message.
func (s *Service) stream(ctx context.Context, client fwv1.EnginePluginClient, req runRequest, upload objstore.MultipartUpload, log *slog.Logger) (*fwv1.BackupResult, error) {
	partURLs := make([]*fwv1.PresignedURL, 0, len(upload.Parts))
	for _, grant := range upload.Parts {
		partURLs = append(partURLs, &fwv1.PresignedURL{
			Url:       grant.URL,
			Method:    grant.Method,
			Headers:   grant.Headers,
			ExpiresAt: timestamppb.New(grant.ExpiresAt),
		})
	}

	stream, err := client.Backup(ctx, &fwv1.BackupRequest{
		Connection:  req.connection.Ref,
		Credentials: req.connection.Credentials,
		BackupId:    req.backupID,
		MethodId:    req.methodID,
		Options:     req.options,
		Databases:   req.databases,
		Timeout:     durationpb.New(runTimeout),
		Target: &fwv1.ArtifactTarget{
			Object:            &fwv1.ObjectRef{Bucket: s.store.Bucket(), Key: upload.Key},
			PartUrls:          partURLs,
			PartSizeBytes:     upload.PartSize,
			ChecksumAlgorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
		},
	})
	if err != nil {
		return nil, pluginError("backup", err)
	}

	var (
		result   *fwv1.BackupResult
		lastLog  time.Time
		terminal bool
	)

	for {
		progress, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, pluginError("backup", err)
		}

		switch progress.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED:
			result = progress.GetResult()
			terminal = true
		case fwv1.JobPhase_JOB_PHASE_FAILED:
			pe := progress.GetError()
			return nil, fmt.Errorf("%w: %s", classifyPluginError(pe), pe.GetMessage())
		case fwv1.JobPhase_JOB_PHASE_CANCELED:
			return nil, fmt.Errorf("the plugin canceled the backup: %s", progress.GetMessage())
		default:
			if time.Since(lastLog) >= progressLogInterval {
				lastLog = time.Now()
				log.InfoContext(ctx, "backup progress",
					slog.String("phase", progress.GetPhase().String()),
					slog.Int64("bytes_written", progress.GetBytesWritten()),
					slog.String("message", progress.GetMessage()))
			}
		}
	}

	// A stream that ended without a terminal message is the one case core cannot interpret: the
	// plugin may have finished, or may have died half way. Treating it as a failure is the only
	// safe reading, because the alternative is a green backup nobody can account for.
	if !terminal || result == nil {
		return nil, fmt.Errorf("the plugin ended the backup stream without reporting an outcome")
	}
	return result, nil
}

// classifyPluginError picks the sentinel for a plugin's structured failure, so a caller can tell an
// unsupported operation from a genuine one without reading the message.
func classifyPluginError(pe *fwv1.PluginError) error {
	if pe.GetCode() == fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
		return ErrUnsupported
	}
	return ErrPluginFailed
}

// pluginError turns a failed plugin RPC into a service error, preferring the structured detail.
func pluginError(operation string, err error) error {
	if pe, ok := sdk.PluginErrorFrom(err); ok {
		return fmt.Errorf("%w: %s: %s", classifyPluginError(pe), operation, pe.GetMessage())
	}
	// Only the status message travels: a transport-level error can carry the target address, and a
	// client is where that must not appear.
	return fmt.Errorf("%w: %s: %s", ErrPluginFailed, operation, status.Convert(err).Message())
}

// -----------------------------------------------------------------------------------------------
// Method selection
// -----------------------------------------------------------------------------------------------

// selectMethod resolves the requested method against what the plugin declares.
//
// Core reads the capability matrix and nothing else. An engine's method list is the plugin's to
// publish, which is what lets a new engine arrive without core learning its name.
func selectMethod(caps *fwv1.Capabilities, requested string) (*fwv1.BackupMethod, error) {
	if len(caps.GetBackupMethods()) == 0 {
		return nil, fmt.Errorf("%w: the %s plugin declares no backup method",
			ErrUnsupported, caps.GetEngineType())
	}
	if requested == "" {
		method := sdk.DefaultBackupMethod(caps)
		if method == nil {
			return nil, fmt.Errorf("%w: the %s plugin declares no default backup method",
				ErrUnsupported, caps.GetEngineType())
		}
		return method, nil
	}

	method := sdk.FindBackupMethod(caps, requested)
	if method == nil {
		return nil, fmt.Errorf("%w: %s has no backup method %q", ErrInvalidArgument, caps.GetEngineType(), requested)
	}
	return method, nil
}

// validateOptions rejects options the method does not declare, and values outside a declared enum.
//
// Rejecting an unknown option rather than passing it through is deliberate: a misspelled option
// that is silently ignored produces a backup taken with settings nobody chose.
func validateOptions(method *fwv1.BackupMethod, options map[string]string) error {
	if len(options) == 0 {
		return nil
	}

	declared := make(map[string]*fwv1.MethodOption, len(method.GetOptions()))
	for _, opt := range method.GetOptions() {
		declared[opt.GetName()] = opt
	}

	for name, value := range options {
		opt, ok := declared[name]
		if !ok {
			return fmt.Errorf("%w: the %s method has no option %q", ErrInvalidArgument, method.GetId(), name)
		}
		if opt.GetType() != fwv1.OptionType_OPTION_TYPE_ENUM {
			continue
		}
		if !contains(opt.GetAllowedValues(), value) {
			return fmt.Errorf("%w: option %q must be one of %v, got %q",
				ErrInvalidArgument, name, opt.GetAllowedValues(), value)
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------------------------
// Persistence
// -----------------------------------------------------------------------------------------------

// createRows inserts the backup that describes this run, and the job it belongs to when there is
// not one already.
//
// The unique index idx_jobs_one_active_per_instance_kind is what makes two concurrent backups of
// one instance impossible. Enforcing it in the database rather than in code is deliberate: two
// simultaneous dumps of one production server is an operational incident, not a race to lose.
//
// A scheduled run arrives with in.JobID already set, because the scheduler created that job in an
// earlier tick and holds a lease on it. Inserting a second job here would collide with that same
// index and fail a backup that was perfectly well scheduled, so the job insert is skipped and the
// backup is attached to the row the lease names.
func (s *Service) createRows(ctx context.Context, instanceID, methodID string, in RunBackupInput) (jobID, backupID string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("backup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	jobID = in.JobID
	if jobID == "" {
		// attempts starts at 1 because this job is created already running: nothing will claim it,
		// so the insert is the only place its one start can be counted.
		err = tx.QueryRow(ctx, `
			INSERT INTO jobs (tenant_id, instance_id, kind, state, started_at, attempts)
			VALUES ($1, $2, 'backup', 'running', now(), 1)
			RETURNING id`,
			s.tenantID, instanceID).Scan(&jobID)
		if metadb.IsUniqueViolation(err) {
			return "", "", fmt.Errorf("%w: %s", ErrAlreadyRunning, instanceID)
		}
		if err != nil {
			return "", "", fmt.Errorf("backup: create job: %w", err)
		}
	}

	// schedule_id is what lets retention and the estate view answer "which recurring intent
	// produced this artifact" after the schedule itself has been edited or deleted.
	var scheduleID any
	if in.ScheduleID != "" {
		scheduleID = in.ScheduleID
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, job_id, schedule_id, method_id, state,
		                     started_at, triggered_manually)
		VALUES ($1, $2, $3, $4, $5, 'running', now(), $6)
		RETURNING id`,
		s.tenantID, instanceID, jobID, scheduleID, methodID, in.TriggeredManually).Scan(&backupID)
	if err != nil {
		return "", "", fmt.Errorf("backup: create backup: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("backup: commit: %w", err)
	}
	return jobID, backupID, nil
}

// recordSuccess writes the artifact's coordinates, the checksum, and the manifest.
func (s *Service) recordSuccess(ctx context.Context, req runRequest, result *fwv1.BackupResult) error {
	manifest, err := protojson.Marshal(result.GetManifest())
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("backup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The engine's own identifier for what was just written, where the engine assigns one.
	//
	// It is what stops this backup being recorded a second time, as somebody else's, by the next
	// observation poll: an engine that keeps a backup history records Fleetward's backups alongside
	// everyone else's, and the upsert in observe.go converges onto this row rather than inserting a
	// duplicate (ADR-0027). NULL rather than empty, because the identity index is partial on it.
	var externalID any
	if id := result.GetExternalId(); id != "" {
		externalID = id
	}

	// The expiry is written here, once, from the retention this run was asked for — never
	// recomputed from the schedule afterwards (ADR-0031).
	//
	// Three consequences follow from stamping rather than computing, and all three are the point.
	// Deleting the schedule cannot change what may be destroyed, because the value is already
	// written. Editing `retention_days` applies from the next backup rather than retroactively
	// destroying artifacts that already exist. And a run with no retention behind it — a manual
	// backup, or any backup taken before this column was ever written — gets NULL, which retention
	// never selects. That last one is why upgrading to this version deletes nothing.
	var expiresAt any
	if stamp := stampedExpiry(req.retentionDays, time.Now()); stamp != nil {
		expiresAt = *stamp
	}

	_, err = tx.Exec(ctx, `
		UPDATE backups
		SET state              = 'succeeded',
		    expires_at         = $14,
		    bucket             = $1,
		    object_key         = $2,
		    size_bytes         = $3,
		    checksum_algorithm = $4,
		    checksum_value     = $5,
		    engine_version     = $6,
		    consistency_point  = $7,
		    manifest           = $8,
		    metadata           = $9,
		    external_id        = $10,
		    completed_at       = now(),
		    duration_ms        = $11,
		    updated_at         = now()
		WHERE id = $12 AND tenant_id = $13`,
		result.GetArtifact().GetBucket(),
		result.GetArtifact().GetKey(),
		result.GetSizeBytes(),
		result.GetChecksum().GetAlgorithm().String(),
		result.GetChecksum().GetValue(),
		result.GetEngineVersion(),
		nullableTime(result.GetConsistencyPoint()),
		manifest,
		metadataOrEmpty(result.GetMetadata()),
		externalID,
		result.GetDuration().AsDuration().Milliseconds(),
		req.backupID, s.tenantID, expiresAt)
	if err != nil {
		return fmt.Errorf("backup: record success: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET state = 'succeeded', finished_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, req.jobID, s.tenantID); err != nil {
		return fmt.Errorf("backup: finish job: %w", err)
	}

	return tx.Commit(ctx)
}

// recordFailure writes why the run failed.
func (s *Service) recordFailure(ctx context.Context, req runRequest, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("backup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	message := cause.Error()
	if _, err := tx.Exec(ctx, `
		UPDATE backups
		SET state = 'failed', error_message = $1, completed_at = now(), updated_at = now()
		WHERE id = $2 AND tenant_id = $3`, message, req.backupID, s.tenantID); err != nil {
		return fmt.Errorf("backup: record failure: %w", err)
	}

	// attempts is not touched here. It counts starts, and this job was already counted — by the
	// scheduler's claim, or by the insert below on the manual path. Incrementing on the way out as
	// well made a scheduled run report two attempts and look like a retry that never happened.
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET state = 'failed', error_message = $1, finished_at = now(), updated_at = now()
		WHERE id = $2 AND tenant_id = $3`, message, req.jobID, s.tenantID); err != nil {
		return fmt.Errorf("backup: fail job: %w", err)
	}

	return tx.Commit(ctx)
}

// GetBackup returns one backup together with the manifest captured when it was taken.
func (s *Service) GetBackup(ctx context.Context, backupID string) (*fwv1.Backup, *fwv1.SourceManifest, error) {
	id, err := requireUUID("backup_id", backupID)
	if err != nil {
		return nil, nil, err
	}

	var manifestRaw []byte
	out, err := s.scanBackup(s.pool.QueryRow(ctx, `
		SELECT `+backupColumns+`, manifest
		FROM backups
		WHERE id = $1 AND tenant_id = $2`, id, s.tenantID), &manifestRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: backup %s", ErrNotFound, backupID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("backup: load backup: %w", err)
	}

	manifest := &fwv1.SourceManifest{}
	if len(manifestRaw) > 0 && string(manifestRaw) != "{}" {
		// A manifest written by an older contract must not make the backup unreadable: the row is
		// still the record that an artifact exists.
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(manifestRaw, manifest); err != nil {
			s.log.WarnContext(ctx, "stored manifest could not be decoded",
				slog.String("backup_id", id), slog.String("error", err.Error()))
			manifest = &fwv1.SourceManifest{}
		}
	}

	// The verification is attached here rather than fetched separately, because "is there a backup"
	// and "is it any good" are the same question to whoever is asking.
	verification, err := s.latestVerification(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	out.Verification = verification

	return out, manifest, nil
}

// ListBackupsInput filters a history listing. Everything is optional; an empty input reports the
// whole estate's most recent backups, of both origins.
type ListBackupsInput struct {
	InstanceID    string
	EnvironmentID string
	State         fwv1.BackupState
	Origin        fwv1.BackupOrigin
	PageSize      int32
}

// ListBackups reports backup history across both origins.
//
// The origin filter defaults to neither, which is deliberate: the question "did this server get
// backed up" does not care who took the backup, and a listing that quietly showed only the backups
// Fleetward took would report an estate with a full year of history as an estate with none
// (ADR-0015).
func (s *Service) ListBackups(ctx context.Context, in ListBackupsInput) ([]*fwv1.Backup, error) {
	size := int(in.PageSize)
	switch {
	case size <= 0:
		size = defaultListPageSize
	case size > maxListPageSize:
		size = maxListPageSize
	}

	args := []any{s.tenantID}
	filters := ""
	add := func(clause, suffix string, value any) {
		args = append(args, value)
		filters += fmt.Sprintf(" AND %s $%d%s", clause, len(args), suffix)
	}

	if in.InstanceID != "" {
		id, err := requireUUID("instance_id", in.InstanceID)
		if err != nil {
			return nil, err
		}
		add("b.instance_id =", "", id)
	}
	if in.EnvironmentID != "" {
		id, err := requireUUID("environment_id", in.EnvironmentID)
		if err != nil {
			return nil, err
		}
		add("b.instance_id IN (SELECT id FROM instances WHERE environment_id =", ")", id)
	}
	if in.State != fwv1.BackupState_BACKUP_STATE_UNSPECIFIED {
		add("b.state =", "", backupStateName(in.State))
	}
	switch in.Origin {
	case fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED:
		add("b.origin =", "", originManaged)
	case fwv1.BackupOrigin_BACKUP_ORIGIN_OBSERVED:
		add("b.origin =", "", originObserved)
	case fwv1.BackupOrigin_BACKUP_ORIGIN_UNSPECIFIED:
		// Both, which is the useful default.
	}

	args = append(args, size)
	// Ordered by when the backup actually happened rather than by when the row was written: an
	// observed backup is recorded whenever the poll ran, which can be days after the fact.
	query := `
		SELECT ` + prefixed(backupColumns, "b.") + `
		FROM backups AS b
		WHERE b.tenant_id = $1` + filters + `
		ORDER BY COALESCE(b.completed_at, b.started_at, b.created_at) DESC, b.id
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("backup: list backups: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.Backup
	for rows.Next() {
		b, err := s.scanBackup(rows, nil)
		if err != nil {
			return nil, fmt.Errorf("backup: read a backup row: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: list backups: %w", err)
	}
	return out, nil
}

// backupColumns is the one place a backup row's projection is written down, so a single backup, a
// listing, and an adherence answer cannot drift into showing different things about the same row.
const backupColumns = `id, instance_id, method_id, state, origin, external_id, external_location,
	       evidence, size_bytes, checksum_algorithm, checksum_value, bucket, object_key,
	       started_at, completed_at, consistency_point, expires_at, duration_ms, error_message,
	       triggered_manually, observed_at`

// prefixed qualifies every column in a projection with a table alias.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// scanner is what both pgx.Row and pgx.Rows satisfy, so one helper reads a single row and a page.
type scanner interface {
	Scan(dest ...any) error
}

// scanBackup reads backupColumns into a contract message. manifestRaw, when non-nil, appends the
// manifest column, which the caller must have selected.
func (s *Service) scanBackup(row scanner, manifestRaw *[]byte) (*fwv1.Backup, error) {
	var (
		out               = &fwv1.Backup{TenantId: s.tenantID}
		state             string
		origin            string
		externalID        *string
		evidenceRaw       []byte
		checksumAlgorithm string
		checksumValue     string
		bucket            string
		objectKey         string
		startedAt         *time.Time
		completedAt       *time.Time
		consistencyPoint  *time.Time
		expiresAt         *time.Time
		observedAt        *time.Time
		durationMS        int64
	)

	dest := []any{
		&out.Id, &out.InstanceId, &out.MethodId, &state, &origin, &externalID, &out.ExternalLocation,
		&evidenceRaw, &out.SizeBytes, &checksumAlgorithm, &checksumValue, &bucket, &objectKey,
		&startedAt, &completedAt, &consistencyPoint, &expiresAt, &durationMS, &out.ErrorMessage,
		&out.TriggeredManually, &observedAt,
	}
	if manifestRaw != nil {
		dest = append(dest, manifestRaw)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}

	out.State = parseBackupState(state)
	out.Origin = parseBackupOrigin(origin)
	if externalID != nil {
		out.ExternalId = *externalID
	}
	out.Duration = durationpb.New(time.Duration(durationMS) * time.Millisecond)
	if checksumValue != "" {
		out.Checksum = &fwv1.Checksum{Algorithm: parseChecksumAlgorithm(checksumAlgorithm), Value: checksumValue}
	}
	if objectKey != "" {
		out.Artifact = &fwv1.ObjectRef{Bucket: bucket, Key: objectKey}
	}
	out.StartedAt = timestampOrNil(startedAt)
	out.CompletedAt = timestampOrNil(completedAt)
	out.ConsistencyPoint = timestampOrNil(consistencyPoint)
	out.ExpiresAt = timestampOrNil(expiresAt)
	out.Evidence = decodeEvidence(evidenceRaw, observedAt)
	return out, nil
}

// -----------------------------------------------------------------------------------------------
// Row helpers
// -----------------------------------------------------------------------------------------------

// artifactKeyFor builds the object key for one backup's artifact.
func artifactKeyFor(tenantID, instanceID, backupID string) string {
	return objstore.ArtifactKey(tenantID, instanceID, backupID, artifactFilename)
}

func parseBackupState(name string) fwv1.BackupState {
	switch name {
	case "pending":
		return fwv1.BackupState_BACKUP_STATE_PENDING
	case "running":
		return fwv1.BackupState_BACKUP_STATE_RUNNING
	case "succeeded":
		return fwv1.BackupState_BACKUP_STATE_SUCCEEDED
	case "failed":
		return fwv1.BackupState_BACKUP_STATE_FAILED
	case "canceled":
		return fwv1.BackupState_BACKUP_STATE_CANCELED
	case "expired":
		return fwv1.BackupState_BACKUP_STATE_EXPIRED
	case "unknown":
		return fwv1.BackupState_BACKUP_STATE_UNKNOWN
	default:
		return fwv1.BackupState_BACKUP_STATE_UNSPECIFIED
	}
}

// backupStateName is the inverse, for the one place a caller filters on a state.
func backupStateName(state fwv1.BackupState) string {
	return strings.ToLower(strings.TrimPrefix(state.String(), "BACKUP_STATE_"))
}

func parseBackupOrigin(name string) fwv1.BackupOrigin {
	if name == originObserved {
		return fwv1.BackupOrigin_BACKUP_ORIGIN_OBSERVED
	}
	return fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED
}

// decodeEvidence reads back what the source a backup was observed from could establish. A managed
// backup has none and gets none: its evidence is the manifest and the checksum.
func decodeEvidence(raw []byte, observedAt *time.Time) *fwv1.ObservedEvidence {
	if len(raw) == 0 || string(raw) == "{}" {
		return nil
	}
	out := &fwv1.ObservedEvidence{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, out); err != nil {
		return nil
	}
	out.ObservedAt = timestampOrNil(observedAt)
	return out
}

// parseChecksumAlgorithm reads back the enum name the column stores. An unknown value becomes
// UNSPECIFIED rather than an error: a checksum whose algorithm cannot be named is still evidence
// worth showing, and refusing to read the row would hide the backup entirely.
func parseChecksumAlgorithm(name string) fwv1.ChecksumAlgorithm {
	if v, ok := fwv1.ChecksumAlgorithm_value[name]; ok {
		return fwv1.ChecksumAlgorithm(v)
	}
	return fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_UNSPECIFIED
}

func timestampOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func nullableTime(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil || !ts.IsValid() {
		return nil
	}
	t := ts.AsTime()
	return &t
}

// metadataOrEmpty keeps a nil map from being written as SQL NULL into a NOT NULL JSONB column.
func metadataOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// requireUUID validates an identifier before it reaches a query, so a typo in a URL is a 400 rather
// than a failed cast reported as a 500.
func requireUUID(field, value string) (string, error) {
	if !metadb.IsUUID(value) {
		return "", fmt.Errorf("%w: %s must be a UUID", ErrInvalidArgument, field)
	}
	return value, nil
}
