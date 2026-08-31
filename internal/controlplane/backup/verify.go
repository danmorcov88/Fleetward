package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/sandbox"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

const (
	// verifyTimeout bounds one verification. It is far shorter than a backup's ceiling: a
	// verification that has not finished in this long is holding a sandbox open, and the sandbox is
	// the resource worth protecting.
	verifyTimeout = 2 * time.Hour

	// downloadTTL is how long the artifact's download grant stays valid. It has to outlive the
	// whole restore, because the plugin fetches the object once the sandbox is up rather than when
	// the grant is minted.
	downloadTTL = 6 * time.Hour
)

// ErrNotVerifiable reports that this backup cannot be verified — it did not succeed, it has no
// artifact, or the plugin serving its engine cannot restore into a sandbox.
var ErrNotVerifiable = errors.New("backup cannot be verified")

// RunVerificationInput describes a manually triggered verification.
type RunVerificationInput struct {
	BackupID string
	// Checks restricts what runs. Empty runs everything the plugin declares.
	Checks []fwv1.VerificationCheck
}

// RunVerification restores a backup into a throwaway sandbox and returns as soon as it has been
// recorded.
//
// Asynchronous for the same reason RunBackup is: pulling an image and starting a database takes
// longer than an HTTP request is allowed to. The caller polls GetVerification.
func (s *Service) RunVerification(ctx context.Context, in RunVerificationInput) (verificationID, jobID string, err error) {
	target, err := s.loadVerificationTarget(ctx, in.BackupID)
	if err != nil {
		return "", "", err
	}

	conn, err := s.resolver.ResolveConnection(ctx, target.instanceID)
	if err != nil {
		return "", "", err
	}

	client, caps, err := s.plugins.Client(conn.EngineType)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, conn.EngineType, err)
	}

	// The capability matrix is the only thing core reads here. An engine that cannot be stood up in
	// a sandbox is reported as unverifiable, which is a legitimate state — quite different from a
	// verification that ran and failed.
	if !caps.GetSupportsSandboxRestore() {
		return "", "", fmt.Errorf("%w: the %s plugin cannot restore into a sandbox",
			ErrUnsupported, conn.EngineType)
	}
	if err := validateChecks(caps, in.Checks); err != nil {
		return "", "", err
	}

	// Resolved before anything is created, so an unusable template fails the request rather than
	// producing a verification row that can only ever be inconclusive.
	image, err := sandbox.ImageRef(caps.GetSandboxTemplate(), target.engineVersion)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s: %w", ErrNotVerifiable, conn.EngineType, err)
	}

	verificationID, jobID, err = s.createVerificationRows(ctx, target, image)
	if err != nil {
		return "", "", err
	}

	s.log.InfoContext(ctx, "verification started",
		slog.String("verification_id", verificationID),
		slog.String("job_id", jobID),
		slog.String("backup_id", target.backupID),
		slog.String("instance_id", target.instanceID),
		slog.String("engine_type", conn.EngineType),
		slog.String("sandbox_image", image))

	s.running.Add(1)
	go func() {
		defer s.running.Done()
		s.verify(client, caps, verifyRequest{
			verificationID: verificationID,
			jobID:          jobID,
			target:         target,
			engineType:     conn.EngineType,
			image:          image,
			checks:         in.Checks,
		})
	}()

	return verificationID, jobID, nil
}

// verificationTarget is everything the run needs from the backup being verified.
type verificationTarget struct {
	backupID      string
	instanceID    string
	methodID      string
	bucket        string
	objectKey     string
	sizeBytes     int64
	checksum      *fwv1.Checksum
	engineVersion string
	manifest      *fwv1.SourceManifest
	// metadata is the plugin's own bookkeeping from the backup — for pg_dump, the artifact's format
	// and source database. Core stores it verbatim and hands it straight back, because which tool
	// loads an artifact is engine knowledge core must not acquire.
	metadata map[string]string
}

// verifyRequest is one verification's inputs, assembled while the caller's context was still alive.
type verifyRequest struct {
	verificationID string
	jobID          string
	target         verificationTarget
	engineType     string
	image          string
	checks         []fwv1.VerificationCheck
}

// loadVerificationTarget reads the backup and refuses one that cannot be verified.
func (s *Service) loadVerificationTarget(ctx context.Context, backupID string) (verificationTarget, error) {
	id, err := requireUUID("backup_id", backupID)
	if err != nil {
		return verificationTarget{}, err
	}

	var (
		out               = verificationTarget{backupID: id}
		state             string
		checksumAlgorithm string
		checksumValue     string
		manifestRaw       []byte
		metadataRaw       []byte
	)

	err = s.pool.QueryRow(ctx, `
		SELECT instance_id, method_id, state, bucket, object_key, size_bytes,
		       checksum_algorithm, checksum_value, engine_version, manifest, metadata
		FROM backups
		WHERE id = $1 AND tenant_id = $2`, id, s.tenantID).
		Scan(&out.instanceID, &out.methodID, &state, &out.bucket, &out.objectKey, &out.sizeBytes,
			&checksumAlgorithm, &checksumValue, &out.engineVersion, &manifestRaw, &metadataRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return verificationTarget{}, fmt.Errorf("%w: backup %s", ErrNotFound, backupID)
	}
	if err != nil {
		return verificationTarget{}, fmt.Errorf("backup: load backup for verification: %w", err)
	}

	if state != "succeeded" {
		return verificationTarget{}, fmt.Errorf("%w: backup %s is %s, and only a succeeded backup has an artifact to restore",
			ErrNotVerifiable, backupID, state)
	}
	if out.objectKey == "" {
		return verificationTarget{}, fmt.Errorf("%w: backup %s records no artifact", ErrNotVerifiable, backupID)
	}
	if checksumValue == "" {
		// Without a checksum there is no way to tell a corrupted artifact from a bad restore, and
		// a verification that cannot make that distinction reports a confidence it has not earned.
		return verificationTarget{}, fmt.Errorf("%w: backup %s records no checksum", ErrNotVerifiable, backupID)
	}

	out.checksum = &fwv1.Checksum{
		Algorithm: parseChecksumAlgorithm(checksumAlgorithm),
		Value:     checksumValue,
	}
	out.manifest = decodeManifest(manifestRaw)
	out.metadata = decodeMetadata(metadataRaw)

	return out, nil
}

// validateChecks rejects a requested check the plugin does not implement.
//
// Core reads the capability matrix and nothing else. Passing an unsupported check through would
// have the plugin refuse mid-run, after a sandbox had already been pulled and started.
func validateChecks(caps *fwv1.Capabilities, checks []fwv1.VerificationCheck) error {
	for _, check := range checks {
		if check == fwv1.VerificationCheck_VERIFICATION_CHECK_UNSPECIFIED {
			return fmt.Errorf("%w: an unspecified verification check was requested", ErrInvalidArgument)
		}
		if !sdk.SupportsCheck(caps, check) {
			return fmt.Errorf("%w: the %s plugin does not implement the %s check",
				ErrUnsupported, caps.GetEngineType(), check)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------------------------

// outcome is what one verification concluded, ready to be written down.
type outcome struct {
	status    fwv1.VerificationStatus
	checks    []*fwv1.CheckResult
	report    string
	errMsg    string
	sandboxID string
}

// verify performs the run and records its outcome. Like execute, it never returns an error: there
// is no caller left to receive one.
func (s *Service) verify(client fwv1.EnginePluginClient, caps *fwv1.Capabilities, req verifyRequest) {
	ctx, cancel := context.WithTimeout(s.runCtx, verifyTimeout)
	defer cancel()

	log := s.log.With(
		slog.String("verification_id", req.verificationID),
		slog.String("backup_id", req.target.backupID))
	started := time.Now()

	result := s.runVerification(ctx, client, caps, req, log)

	// Detached from the run's context: a verification cancelled by shutdown or by its timeout must
	// still be able to write down what it concluded.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer recordCancel()

	if err := s.recordVerification(recordCtx, req, result, time.Since(started)); err != nil {
		log.ErrorContext(recordCtx, "could not record the verification outcome",
			slog.String("status", result.status.String()),
			slog.String("error", err.Error()))
		return
	}

	switch result.status {
	case fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED:
		log.InfoContext(recordCtx, "backup verified",
			slog.Duration("duration", time.Since(started)))
	case fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED:
		// The loudest line this product writes. A backup believed good and proven bad is more
		// dangerous than a backup known to be missing.
		log.ErrorContext(recordCtx, "backup verification FAILED",
			slog.String("report", result.report),
			slog.Duration("duration", time.Since(started)))
	default:
		log.WarnContext(recordCtx, "backup verification was inconclusive",
			slog.String("reason", result.errMsg),
			slog.Duration("duration", time.Since(started)))
	}
}

// runVerification provisions the sandbox, drives the plugin, and returns what it concluded.
//
// Every failure that is not evidence about the backup produces INCONCLUSIVE. That distinction is
// the whole point of having two failure statuses: an image that could not be pulled and a table
// that came back short are both "verification did not succeed", and treating them the same trains
// an operator to ignore the one alert that matters.
func (s *Service) runVerification(ctx context.Context, client fwv1.EnginePluginClient, caps *fwv1.Capabilities, req verifyRequest, log *slog.Logger) outcome {
	if len(req.target.manifest.GetEntries()) == 0 {
		// Comparing zero objects to zero objects succeeds trivially. Reporting VERIFIED here would
		// be the single most dangerous answer this service can give, so an absent manifest is
		// inconclusive by construction and never reaches a sandbox.
		return inconclusiveOutcome("the backup carries no manifest, so a restore of it would prove nothing")
	}
	if s.sandboxes == nil {
		return inconclusiveOutcome("no sandbox provider is configured on this control plane")
	}

	grant, err := s.store.PresignGet(ctx, req.target.objectKey, downloadTTL)
	if err != nil {
		return inconclusiveOutcome("the artifact's download grant could not be issued: %v", err)
	}

	artifact := &fwv1.ArtifactSource{
		Object: &fwv1.ObjectRef{Bucket: req.target.bucket, Key: req.target.objectKey},
		DownloadUrl: &fwv1.PresignedURL{
			Url:       grant.URL,
			Method:    grant.Method,
			Headers:   grant.Headers,
			ExpiresAt: timestamppb.New(grant.ExpiresAt),
		},
		Checksum:  req.target.checksum,
		Role:      fwv1.ArtifactRole_ARTIFACT_ROLE_BASE,
		SizeBytes: req.target.sizeBytes,
	}

	spec := sandbox.Spec{
		EngineType: req.engineType,
		// The version that produced the artifact, not the version the instance runs today. An
		// instance can be upgraded between a backup and its verification while the artifact still
		// restores as whatever wrote it, and restoring into the wrong major version fails in ways
		// that look exactly like data corruption.
		EngineVersion: req.target.engineVersion,
		Template:      caps.GetSandboxTemplate(),
		Labels: map[string]string{
			"fleetward.verification_id": req.verificationID,
			"fleetward.backup_id":       req.target.backupID,
		},
	}

	result := inconclusiveOutcome("the verification did not run")

	// sandbox.Run is the guarantee: the container is destroyed on every path out of the closure,
	// including a panic, with a context that is not this one — a verification usually fails by
	// cancellation, and a cancelled context cannot clean up.
	runErr := sandbox.Run(ctx, s.sandboxes, spec, func(ctx context.Context, box sandbox.Sandbox) error {
		result = s.restoreAndVerify(ctx, client, req, artifact, box, log)
		return nil
	})
	if runErr != nil {
		if result.sandboxID == "" {
			// Provisioning never got as far as handing us a sandbox: an image that could not be
			// pulled, a container that never became ready, a Docker daemon that is not there.
			return inconclusiveOutcome("the verification sandbox could not be provisioned: %v", runErr)
		}
		// The checks ran; only the teardown failed. The conclusion still stands, but a container
		// may be leaking, which an operator needs to know about.
		log.ErrorContext(ctx, "a verification sandbox may have leaked",
			slog.String("sandbox_id", result.sandboxID),
			slog.String("error", runErr.Error()))
	}

	return result
}

// restoreAndVerify drives the two plugin calls against a live sandbox.
func (s *Service) restoreAndVerify(ctx context.Context, client fwv1.EnginePluginClient, req verifyRequest, artifact *fwv1.ArtifactSource, box sandbox.Sandbox, log *slog.Logger) outcome {
	target := &fwv1.RestoreTarget{
		Kind:        fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX,
		Credentials: box.Credentials(),
		SandboxId:   box.ID(),
	}

	if err := s.restore(ctx, client, req, artifact, target, log); err != nil {
		result := classifyRestoreOutcome(err)
		result.sandboxID = box.ID()
		return result
	}

	verified, err := client.VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
		VerificationId: req.verificationID,
		Target:         target,
		Expected:       req.target.manifest,
		Checks:         req.checks,
		BackupId:       req.target.backupID,
		Timeout:        durationpb.New(verifyTimeout),
	})
	if err != nil {
		// A failed VerifyRestore call is the machinery failing, not an answer about the data. The
		// plugin reports a genuine mismatch inside a successful response.
		return outcome{
			status:    fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
			errMsg:    pluginError("verify restore", err).Error(),
			report:    "verification could not reach a conclusion: the plugin's checks did not run",
			sandboxID: box.ID(),
		}
	}

	status := verified.GetStatus()
	if status == fwv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED {
		// A plugin that answers without a status has said nothing, and reading that as VERIFIED
		// would be exactly the wrong default.
		status = fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE
	}

	return outcome{
		status:    status,
		checks:    verified.GetChecks(),
		report:    verified.GetReport(),
		errMsg:    verified.GetError().GetMessage(),
		sandboxID: box.ID(),
	}
}

// restore drives the plugin's Restore RPC to its terminal message.
func (s *Service) restore(ctx context.Context, client fwv1.EnginePluginClient, req verifyRequest, artifact *fwv1.ArtifactSource, target *fwv1.RestoreTarget, log *slog.Logger) error {
	stream, err := client.Restore(ctx, &fwv1.RestoreRequest{
		RestoreId: req.verificationID,
		Artifacts: []*fwv1.ArtifactSource{artifact},
		Target:    target,
		MethodId:  req.target.methodID,
		// The backup's own metadata, handed straight back. It is how the plugin knows which of its
		// tools loads this artifact, and core never inspects it.
		Options: req.target.metadata,
		Timeout: durationpb.New(verifyTimeout),
	})
	if err != nil {
		return pluginError("restore", err)
	}

	var (
		lastLog  time.Time
		terminal bool
	)

	for {
		progress, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pluginError("restore", err)
		}

		switch progress.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED:
			terminal = true
		case fwv1.JobPhase_JOB_PHASE_FAILED:
			pe := progress.GetError()
			return &restoreError{plugin: pe}
		case fwv1.JobPhase_JOB_PHASE_CANCELED:
			return fmt.Errorf("the plugin canceled the restore: %s", progress.GetMessage())
		default:
			if time.Since(lastLog) >= progressLogInterval {
				lastLog = time.Now()
				log.InfoContext(ctx, "restore progress",
					slog.String("phase", progress.GetPhase().String()),
					slog.Int64("bytes_read", progress.GetBytesRead()),
					slog.String("message", progress.GetMessage()))
			}
		}
	}

	if !terminal {
		return fmt.Errorf("the plugin ended the restore stream without reporting an outcome")
	}
	return nil
}

// restoreError carries a plugin's structured failure from a restore, so the classification below
// can read its code and details rather than parse a string.
type restoreError struct {
	plugin *fwv1.PluginError
}

func (e *restoreError) Error() string {
	return fmt.Sprintf("%s: %s", e.plugin.GetCode(), e.plugin.GetMessage())
}

// classifyRestoreOutcome decides whether a failed restore is evidence about the backup.
//
// FAILED means the artifact is bad: it does not match the checksum recorded with it, or the
// engine's own tooling could not load it. Everything else — a missing tool, an unreachable sandbox,
// a transfer that broke, a plugin that refused — is INCONCLUSIVE, because none of it says anything
// about whether the backup is restorable.
func classifyRestoreOutcome(err error) outcome {
	var re *restoreError
	if !errors.As(err, &re) {
		return inconclusiveOutcome("the restore did not complete: %v", err)
	}

	pe := re.plugin
	corrupt := sdk.IsArtifactCorrupt(pe)

	if RestoreFailureStatus(pe) == fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
		detail := "the engine's own tooling could not load the artifact"
		if corrupt {
			detail = "the artifact is not the one that was written"
		}
		return outcome{
			status: fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED,
			errMsg: pe.GetMessage(),
			checks: []*fwv1.CheckResult{{
				// Reported as a failed connectivity check because the restored instance never came
				// to exist: none of the data checks could even be attempted.
				Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
				Passed:   false,
				Severity: fwv1.Severity_SEVERITY_CRITICAL,
				Message:  pe.GetMessage(),
			}},
			report: "the backup could not be restored: " + detail + "\n" + pe.GetMessage(),
		}
	}

	return inconclusiveOutcome("the restore did not complete: %s", pe.GetMessage())
}

// RestoreFailureStatus is the rule ADR-0022 states, in one place: which restore failures are
// evidence about the backup, and which are evidence about everything else.
//
// FAILED means the artifact is bad — it is not the one that was written, or the engine's own
// tooling could not load it. Every other failure is INCONCLUSIVE, because none of it says anything
// about whether the backup is restorable.
//
// It is exported so that the plugin conformance suite can assert the outcome a plugin's error
// actually produces, rather than a proxy for it. A plugin that reports an unreachable sandbox as a
// tool failure would pass a check written against the proxy and still fire this product's one
// critical alert on a container that lost a race.
func RestoreFailureStatus(pe *fwv1.PluginError) fwv1.VerificationStatus {
	if sdk.IsArtifactCorrupt(pe) || pe.GetCode() == fwv1.ErrorCode_ERROR_CODE_TOOL_FAILED {
		return fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED
	}
	return fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE
}

// inconclusiveOutcome builds the answer for a verification that could not be reached.
func inconclusiveOutcome(format string, args ...any) outcome {
	message := fmt.Sprintf(format, args...)
	return outcome{
		status: fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		errMsg: message,
		report: "verification could not reach a conclusion: " + message,
	}
}

// -----------------------------------------------------------------------------------------------
// Persistence
// -----------------------------------------------------------------------------------------------

// createVerificationRows inserts the job and the verification that describe this run.
//
// The same unique index that keeps two backups of one instance apart keeps two verifications apart,
// because a second concurrent restore would double the sandbox load for no extra information.
func (s *Service) createVerificationRows(ctx context.Context, target verificationTarget, image string) (verificationID, jobID string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("verify: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (tenant_id, instance_id, kind, state, started_at)
		VALUES ($1, $2, 'verify', 'running', now())
		RETURNING id`,
		s.tenantID, target.instanceID).Scan(&jobID)
	if metadb.IsUniqueViolation(err) {
		return "", "", fmt.Errorf("%w: a verification is already running for %s", ErrAlreadyRunning, target.instanceID)
	}
	if err != nil {
		return "", "", fmt.Errorf("verify: create job: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO verifications (tenant_id, backup_id, job_id, status, sandbox_image, started_at)
		VALUES ($1, $2, $3, 'running', $4, now())
		RETURNING id`,
		s.tenantID, target.backupID, jobID, image).Scan(&verificationID)
	if err != nil {
		return "", "", fmt.Errorf("verify: create verification: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("verify: commit: %w", err)
	}
	return verificationID, jobID, nil
}

// recordVerification writes the outcome and closes the job.
//
// The job succeeds whenever the verification reached a conclusion, including a conclusion of
// FAILED: the job is "did we manage to check", the verification is "was the backup good". Collapsing
// the two would make a failed backup indistinguishable from a broken control plane in the job table.
func (s *Service) recordVerification(ctx context.Context, req verifyRequest, result outcome, elapsed time.Duration) error {
	checks, err := encodeCheckResults(result.checks)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("verify: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE verifications
		SET status        = $1,
		    checks        = $2,
		    report        = $3,
		    sandbox_id    = $4,
		    error_message = $5,
		    completed_at  = now(),
		    duration_ms   = $6,
		    updated_at    = now()
		WHERE id = $7 AND tenant_id = $8`,
		verificationStateName(result.status), checks, result.report, result.sandboxID, result.errMsg,
		elapsed.Milliseconds(), req.verificationID, s.tenantID); err != nil {
		return fmt.Errorf("verify: record outcome: %w", err)
	}

	jobState := "succeeded"
	if result.status == fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
		jobState = "failed"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET state = $1, error_message = $2, finished_at = now(), attempts = attempts + 1, updated_at = now()
		WHERE id = $3 AND tenant_id = $4`,
		jobState, result.errMsg, req.jobID, s.tenantID); err != nil {
		return fmt.Errorf("verify: finish job: %w", err)
	}

	return tx.Commit(ctx)
}

// GetVerification returns one verification and everything it concluded.
func (s *Service) GetVerification(ctx context.Context, verificationID string) (*fwv1.Verification, error) {
	id, err := requireUUID("verification_id", verificationID)
	if err != nil {
		return nil, err
	}

	var (
		out         = &fwv1.Verification{Id: id}
		status      string
		checksRaw   []byte
		startedAt   *time.Time
		completedAt *time.Time
		durationMS  int64
	)

	err = s.pool.QueryRow(ctx, `
		SELECT backup_id, status, checks, report, error_message, started_at, completed_at, duration_ms
		FROM verifications
		WHERE id = $1 AND tenant_id = $2`, id, s.tenantID).
		Scan(&out.BackupId, &status, &checksRaw, &out.Report, &out.ErrorMessage,
			&startedAt, &completedAt, &durationMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: verification %s", ErrNotFound, verificationID)
	}
	if err != nil {
		return nil, fmt.Errorf("backup: load verification: %w", err)
	}

	out.Status = parseVerificationStatus(status)
	out.Duration = durationpb.New(time.Duration(durationMS) * time.Millisecond)
	out.StartedAt = timestampOrNil(startedAt)
	out.CompletedAt = timestampOrNil(completedAt)
	out.Checks = s.decodeCheckResults(ctx, id, checksRaw)

	return out, nil
}

// latestVerification returns the most recent verification of one backup, or nil if it has never
// been verified.
//
// Backup state and verification state are deliberately two facts rather than one column. "Backed up
// but never verified" and "backed up and proven bad" look identical if they are collapsed, and the
// second is the more dangerous of the two.
func (s *Service) latestVerification(ctx context.Context, backupID string) (*fwv1.Verification, error) {
	var (
		out         = &fwv1.Verification{BackupId: backupID}
		status      string
		checksRaw   []byte
		startedAt   *time.Time
		completedAt *time.Time
		durationMS  int64
	)

	err := s.pool.QueryRow(ctx, `
		SELECT id, status, checks, report, error_message, started_at, completed_at, duration_ms
		FROM verifications
		WHERE backup_id = $1 AND tenant_id = $2
		ORDER BY created_at DESC
		LIMIT 1`, backupID, s.tenantID).
		Scan(&out.Id, &status, &checksRaw, &out.Report, &out.ErrorMessage,
			&startedAt, &completedAt, &durationMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // "never verified" is an absence, not a failure
	}
	if err != nil {
		return nil, fmt.Errorf("backup: load latest verification: %w", err)
	}

	out.Status = parseVerificationStatus(status)
	out.Duration = durationpb.New(time.Duration(durationMS) * time.Millisecond)
	out.StartedAt = timestampOrNil(startedAt)
	out.CompletedAt = timestampOrNil(completedAt)
	out.Checks = s.decodeCheckResults(ctx, out.GetId(), checksRaw)

	return out, nil
}

// -----------------------------------------------------------------------------------------------
// Encoding helpers
// -----------------------------------------------------------------------------------------------

// encodeCheckResults renders the per-check results as the JSONB array the column holds.
//
// Built element by element rather than through a wrapper message so that the stored document is a
// plain array — readable with jsonb_array_length in a psql session, which is how an operator will
// actually look at it.
func encodeCheckResults(checks []*fwv1.CheckResult) ([]byte, error) {
	if len(checks) == 0 {
		return []byte("[]"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, check := range checks {
		if i > 0 {
			buf.WriteByte(',')
		}
		encoded, err := protojson.Marshal(check)
		if err != nil {
			return nil, fmt.Errorf("verify: encode check result: %w", err)
		}
		buf.Write(encoded)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// decodeCheckResults reads the stored array back.
//
// A document written by an older contract must not make the verification unreadable: the row is
// still the record that a verification happened and what it concluded.
func (s *Service) decodeCheckResults(ctx context.Context, verificationID string, raw []byte) []*fwv1.CheckResult {
	if len(raw) == 0 || string(raw) == "[]" {
		return nil
	}

	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		s.log.WarnContext(ctx, "stored verification checks could not be decoded",
			slog.String("verification_id", verificationID), slog.String("error", err.Error()))
		return nil
	}

	checks := make([]*fwv1.CheckResult, 0, len(elements))
	for _, element := range elements {
		check := &fwv1.CheckResult{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(element, check); err != nil {
			s.log.WarnContext(ctx, "a stored verification check could not be decoded",
				slog.String("verification_id", verificationID), slog.String("error", err.Error()))
			continue
		}
		checks = append(checks, check)
	}
	return checks
}

// decodeManifest reads a backup's stored manifest, tolerating one written by an older contract.
func decodeManifest(raw []byte) *fwv1.SourceManifest {
	manifest := &fwv1.SourceManifest{}
	if len(raw) == 0 || string(raw) == "{}" {
		return manifest
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, manifest); err != nil {
		return &fwv1.SourceManifest{}
	}
	return manifest
}

// verificationStateName maps the contract's status onto the column's vocabulary.
func verificationStateName(status fwv1.VerificationStatus) string {
	switch status {
	case fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED:
		return "verified"
	case fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED:
		return "failed"
	default:
		return "inconclusive"
	}
}

// parseVerificationStatus reads the column back.
func parseVerificationStatus(name string) fwv1.VerificationStatus {
	switch name {
	case "verified":
		return fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED
	case "failed":
		return fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED
	case "inconclusive":
		return fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE
	default:
		// 'pending' and 'running' have no contract equivalent: the status field describes a
		// conclusion, and there is not one yet.
		return fwv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED
	}
}

// decodeMetadata reads a backup's stored plugin metadata. Core never interprets it — it exists so
// the plugin that wrote an artifact can tell, at restore time, what it wrote.
func decodeMetadata(raw []byte) map[string]string {
	if len(raw) == 0 || string(raw) == "{}" {
		return nil
	}
	metadata := map[string]string{}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil
	}
	return metadata
}
