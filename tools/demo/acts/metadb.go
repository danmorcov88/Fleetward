package acts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// metadb is the one place the demo goes around the API, and it does exactly one thing: insert
// backups that happened in the past.
//
// There is no endpoint for that and there should not be — you cannot ask Fleetward to have taken a
// backup last Tuesday, and an API that let you would be a way to forge evidence about an estate.
// So the fixture is written as rows, it is labelled as seeded everywhere it is shown, and it is the
// only thing in this package that touches the database.
//
// Two rules hold over everything written here:
//
//   - **Seeded backups carry no expiry.** NULL means never expires and nothing recomputes it
//     (ADR-0031). Anything else would let the hourly retention sweep quietly delete six weeks of
//     history in front of an audience. Exactly five rows on exactly one instance are stamped, and
//     that is what acts 5 and 6 are built out of.
//   - **Seeded timestamps are relative to now.** A fixture seeded with fixed dates reads as stale a
//     week later and as broken a year later.
type metadb struct {
	pool   *pgxpool.Pool
	tenant string
}

func openMetadb(ctx context.Context, cfg Config) (*metadb, error) {
	pool, err := pgxpool.New(ctx, cfg.MetaDSN)
	if err != nil {
		return nil, fmt.Errorf("connect to the metadata database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, wrongDatabase(cfg, err)
	}

	// Not just a ping: a developer machine often already runs a PostgreSQL, and on Windows both it
	// and Docker's port proxy can bind 5432 at once — so a successful connection is not proof that
	// the thing answering is the stack's metadata store. Asking for the tenant migration 000001
	// seeds is.
	var tenant string
	if err := pool.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = 'default'`).Scan(&tenant); err != nil {
		pool.Close()
		return nil, wrongDatabase(cfg, err)
	}
	return &metadb{pool: pool, tenant: tenant}, nil
}

// wrongDatabase turns "the connection did not work" into the thing to change.
//
// The failure this exists for is not exotic: a machine that already runs PostgreSQL has something on
// 5432 that is not the stack, and the error a driver produces for that reads like a broken
// installation rather than like a port collision. Every published port can be moved from .env, and
// the demo reads the same variables the stack does.
func wrongDatabase(cfg Config, cause error) error {
	return fmt.Errorf("the metadata database at %s did not answer as Fleetward's: %w\n\n"+
		"If this machine already runs a PostgreSQL, that is what answered rather than the stack's — "+
		"on Windows both it and Docker's port proxy can bind 5432 at once. Move the published port "+
		"and run again:\n\n"+
		"  echo FLEETWARD_POSTGRES_PORT=55432 >> .env",
		cfg.MetaDSN, cause)
}

func (m *metadb) Close() {
	if m != nil && m.pool != nil {
		m.pool.Close()
	}
}

// resetDemoEstate removes what a previous run left, so running the demo twice is not confusing.
//
// It deletes the fixture and nothing else: environments whose name carries the demo prefix, the
// instances inside them, and the credentials issued to the two demo users. Backups, schedules,
// jobs, verifications and connections all cascade from the instance.
//
// The two demo *users* deliberately survive. `audit_log.user_id` references them with ON DELETE SET
// NULL, and audit_log refuses UPDATE at the database level — which is the entire point of the
// trigger — so deleting a user who appears in the log would fail, correctly. Re-issuing a token to
// the same email reuses the user, which is what the API already does.
func (m *metadb) resetDemoEstate(ctx context.Context) error {
	statements := []string{
		`DELETE FROM instances
		 WHERE tenant_id = $1
		   AND environment_id IN (SELECT id FROM environments WHERE tenant_id = $1 AND name LIKE 'demo-%')`,
		`DELETE FROM environments WHERE tenant_id = $1 AND name LIKE 'demo-%'`,
		`DELETE FROM api_tokens
		 WHERE tenant_id = $1
		   AND user_id IN (SELECT id FROM users WHERE tenant_id = $1 AND email LIKE 'demo-%@fleetward.dev')`,
		`DELETE FROM role_grants
		 WHERE tenant_id = $1
		   AND user_id IN (SELECT id FROM users WHERE tenant_id = $1 AND email LIKE 'demo-%@fleetward.dev')`,
	}
	for _, sql := range statements {
		if _, err := m.pool.Exec(ctx, sql, m.tenant); err != nil {
			return fmt.Errorf("clear the previous demo estate: %w", err)
		}
	}
	return nil
}

// seedCounts is what the narrator reports, so the screen says how much of what it shows is fixture.
type seedCounts struct {
	backups  int
	verified int
	observed int
}

// The shape of the seeded history.
const (
	// historyDays is six weeks: long enough that the estate view looks like an estate with real
	// problems rather than a fresh install.
	historyDays = 42
	// nightlyAt is when a seeded backup finished — seven minutes after the 02:00 it was expected
	// by, comfortably inside the two hours of grace the schedule declares.
	nightlyHour   = 2
	nightlyMinute = 7
	// fortnight is how long mssql-archive-prod has been backing up successfully and failing
	// verification, which is the case retention's floor exists for (ADR-0032).
	fortnight = 14
)

// seedHistory writes the fixture and returns what it wrote.
func (m *metadb) seedHistory(ctx context.Context, s *seeded) (seedCounts, error) {
	var counts seedCounts
	now := time.Now().UTC()

	for _, inst := range estate {
		id := s.instances[inst.name]
		if id == "" || inst.history == historyNone {
			continue
		}
		for day := 0; day < daysFor(inst.history); day++ {
			at := nightly(now, day)
			if at.After(now) || skipDay(inst.history, day) {
				continue
			}
			written, err := m.seedOneBackup(ctx, id, inst, at, day)
			if err != nil {
				return counts, err
			}
			counts.backups++
			counts.verified += written.verified
			counts.observed += written.observed
		}
	}
	return counts, nil
}

// seedOutcome is what one seeded night produced.
type seedOutcome struct {
	verified int
	observed int
}

func (m *metadb) seedOneBackup(
	ctx context.Context, instanceID string, inst seededInstance, at time.Time, day int,
) (seedOutcome, error) {
	backupID := newUUID()
	started := at.Add(-6 * time.Minute)
	size := 40<<20 + int64(day)*137<<10

	if inst.history == historyObserved || inst.history == historyObservedUnproven {
		return seedOutcome{observed: 1}, m.insertObserved(ctx, backupID, instanceID, inst, at, started, size)
	}
	if err := m.insertManaged(ctx, backupID, instanceID, inst, at, started, size); err != nil {
		return seedOutcome{}, err
	}

	status, report := verdictFor(inst.history, day)
	if status == "" {
		return seedOutcome{}, nil
	}
	if err := m.insertVerification(ctx, backupID, at, status, report); err != nil {
		return seedOutcome{}, err
	}
	if status == "verified" {
		return seedOutcome{verified: 1}, nil
	}
	return seedOutcome{}, nil
}

// insertManaged writes a backup Fleetward is recorded as having taken.
//
// bucket and object_key carry the real key shape because that is what a real row looks like, and
// nothing behind them exists: the seeded history is metadata, and docs/demo.md says so. Deleting an
// object that is not there is not an error, which is what makes that safe even if act 6's expiry
// stamping reaches one of these rows.
func (m *metadb) insertManaged(
	ctx context.Context, backupID, instanceID string, inst seededInstance,
	at, started time.Time, size int64,
) error {
	key := fmt.Sprintf("tenants/%s/instances/%s/backups/%s/artifact", m.tenant, instanceID, backupID)
	_, err := m.pool.Exec(ctx, `
		INSERT INTO backups (
		    id, tenant_id, instance_id, method_id, state, origin,
		    bucket, object_key, size_bytes, checksum_algorithm, checksum_value,
		    consistency_point, started_at, completed_at, duration_ms, expires_at, created_at, updated_at
		) VALUES (
		    $1, $2, $3, $4, 'succeeded', 'managed',
		    'fleetward-backups', $5, $6, 'CHECKSUM_ALGORITHM_SHA256', $7,
		    $8, $9, $8, $10, NULL, $8, $8
		)`,
		backupID, m.tenant, instanceID, methodFor(inst.engine),
		key, size, randomHex(32),
		at, started, at.Sub(started).Milliseconds())
	if err != nil {
		return fmt.Errorf("seed a backup for %s: %w", inst.name, err)
	}
	return nil
}

// insertObserved writes evidence about a backup somebody else took.
//
// `bucket` and `object_key` stay empty by construction: they mean "an object Fleetward owns", and a
// screen offering a download of somebody else's file would be lying (ADR-0015). The evidence column
// carries what the source could and could not establish, which is what the caveats on the estate
// view are rendered from.
func (m *metadb) insertObserved(
	ctx context.Context, backupID, instanceID string, inst seededInstance,
	at, started time.Time, size int64,
) error {
	state, evidence := "succeeded", `{
		"source_description": "the backup catalogue this engine keeps",
		"reports_outcome": true,
		"identity_is_engine_assigned": true
	}`
	if inst.history == historyObservedUnproven {
		// A file exists and nothing about it says the dump that wrote it finished. That is a real
		// answer, and it is neither success nor failure.
		state, evidence = "unknown", `{
			"source_description": "a directory of dump files, which assigns no identity and reports no outcome",
			"reports_outcome": false,
			"identity_is_engine_assigned": false
		}`
	}

	external := fmt.Sprintf("%s-%s.dump", inst.name, at.Format("20060102"))
	_, err := m.pool.Exec(ctx, `
		INSERT INTO backups (
		    id, tenant_id, instance_id, method_id, state, origin,
		    external_id, external_location, evidence, observed_at,
		    size_bytes, started_at, completed_at, duration_ms, expires_at, created_at, updated_at
		) VALUES (
		    $1, $2, $3, $4, $5, 'observed',
		    $6, $7, $8::jsonb, $9,
		    $10, $11, $9, $12, NULL, $9, $9
		)`,
		backupID, m.tenant, instanceID, methodFor(inst.engine), state,
		external, "/var/backups/"+external, evidence, at,
		size, started, at.Sub(started).Milliseconds())
	if err != nil {
		return fmt.Errorf("seed an observed backup for %s: %w", inst.name, err)
	}
	return nil
}

func (m *metadb) insertVerification(ctx context.Context, backupID string, at time.Time, status, report string) error {
	completed := at.Add(4 * time.Minute)
	checks := verificationChecksJSON(status, report)
	_, err := m.pool.Exec(ctx, `
		INSERT INTO verifications (
		    id, tenant_id, backup_id, status, checks, report,
		    started_at, completed_at, duration_ms, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $8, $8)`,
		newUUID(), m.tenant, backupID, status, checks, report,
		at, completed, completed.Sub(at).Milliseconds())
	if err != nil {
		return fmt.Errorf("seed a verification: %w", err)
	}
	return nil
}

// stampExpiry puts an expiry in the past on one seeded backup, which is how act 6 gets something to
// have an opinion about without waiting fourteen days for the schedule's retention to arrive.
func (m *metadb) stampExpiry(ctx context.Context, backupID string, ago time.Duration) error {
	_, err := m.pool.Exec(ctx,
		`UPDATE backups SET expires_at = now() - $2::interval WHERE id = $1 AND tenant_id = $3`,
		backupID, fmt.Sprintf("%d seconds", int(ago.Seconds())), m.tenant)
	if err != nil {
		return fmt.Errorf("stamp an expiry: %w", err)
	}
	return nil
}

// backupsOf lists the seeded backups of one instance, newest first, with the verdict each one
// reached. Act 6 uses it to pick the rows whose expiry it stamps.
func (m *metadb) backupsOf(ctx context.Context, instanceID string) ([]seededBackup, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT b.id,
		       COALESCE((SELECT v.status FROM verifications v
		                 WHERE v.backup_id = b.id ORDER BY v.created_at DESC LIMIT 1), '')
		FROM   backups b
		WHERE  b.tenant_id = $1 AND b.instance_id = $2 AND b.origin = 'managed' AND b.state = 'succeeded'
		ORDER  BY b.completed_at DESC`, m.tenant, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read seeded backups: %w", err)
	}
	defer rows.Close()

	var out []seededBackup
	for rows.Next() {
		var b seededBackup
		if err := rows.Scan(&b.id, &b.verdict); err != nil {
			return nil, fmt.Errorf("read a seeded backup: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type seededBackup struct {
	id      string
	verdict string
}

// -----------------------------------------------------------------------------------------------
// The shape of the fixture
// -----------------------------------------------------------------------------------------------

func daysFor(h historyKind) int {
	if h == historyUnverifiable {
		return fortnight
	}
	return historyDays
}

// skipDay is what makes an instance behind its window: its history stops three days ago, so the
// occurrence currently under judgement has nothing in it.
func skipDay(h historyKind, day int) bool {
	return h == historyBehind && day < 3
}

func nightly(now time.Time, daysAgo int) time.Time {
	day := now.AddDate(0, 0, -daysAgo)
	return time.Date(day.Year(), day.Month(), day.Day(), nightlyHour, nightlyMinute, 0, 0, time.UTC)
}

// verdictFor is what verification concluded about a seeded backup, and it is where the fixture's
// most useful row comes from: an instance whose backups have succeeded every night for a fortnight
// and been restorable on only one of them.
func verdictFor(h historyKind, day int) (status, report string) {
	switch h {
	case historyUnverifiable:
		if day == fortnight-1 {
			return "verified", "restored into a sandbox and matched the manifest exactly"
		}
		return "failed", "record counts do not match the manifest: dbo.Orders expected 148932 rows, " +
			"found 148120. The artifact restored cleanly and it is not what it claims to be."
	case historyHealthy, historyBehind:
		return "verified", "restored into a sandbox and matched the manifest exactly"
	default:
		return "", ""
	}
}

// verificationChecksJSON renders one check result in the shape the API reads back, which is
// protojson — so a seeded verification renders through exactly the same code path a real one does.
func verificationChecksJSON(status, message string) string {
	passed, severity := "true", "SEVERITY_INFO"
	if status != "verified" {
		passed, severity = "false", "SEVERITY_CRITICAL"
	}
	return fmt.Sprintf(`[{
		"check": "VERIFICATION_CHECK_RECORD_COUNTS",
		"passed": %s,
		"severity": "%s",
		"message": %q
	}]`, passed, severity, message)
}

// newUUID returns a version 4 UUID.
//
// Written here rather than pulled in as a dependency: the demo needs an identifier before the row
// exists, because a managed backup's object key contains it, and that is the whole requirement.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("demo: no randomness available: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("demo: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// stampSeededExpiries gives retention something to do, and something to refuse to do.
//
// It stamps five of one instance's fourteen seeded backups with an expiry that has already passed:
// the newest, the one verification proved restorable, and three from the middle. The floor protects
// the first two forever; the other three are what the next sweep deletes, and deleting them is what
// puts rows under `system:retention` in the audit log for act 5 to read.
//
// Nothing else about the rows is touched, and no other instance is stamped. Every remaining seeded
// backup keeps expires_at NULL, which is what stops an hourly sweep from quietly blanking six weeks
// of history in front of an audience (ADR-0031).
func (m *metadb) stampSeededExpiries(ctx context.Context, instanceID string) (int, error) {
	rows, err := m.backupsOf(ctx, instanceID)
	if err != nil {
		return 0, err
	}
	if len(rows) < 10 {
		return 0, fmt.Errorf("expected a fortnight of seeded backups to stamp, found %d", len(rows))
	}

	stamp := map[string]bool{rows[0].id: true}
	for _, r := range rows {
		if r.verdict == "verified" {
			stamp[r.id] = true
			break
		}
	}
	for _, i := range []int{5, 6, 7} {
		stamp[rows[i].id] = true
	}

	for id := range stamp {
		if err := m.stampExpiry(ctx, id, 48*time.Hour); err != nil {
			return 0, err
		}
	}
	return len(stamp), nil
}

// stampUnprotectedExpiry puts one more expiry in the past, on a backup the floor does not protect.
//
// Act 6 calls it immediately before asking for the preview, so the answer has a row in both columns:
// one artifact the next sweep will remove, and the ones it will not. Without it the act would show
// only what is kept, because the sweep that runs every minute in the development stack has usually
// already taken the rest.
func (m *metadb) stampUnprotectedExpiry(ctx context.Context, instanceID string) error {
	rows, err := m.backupsOf(ctx, instanceID)
	if err != nil {
		return err
	}
	if len(rows) < 5 {
		return fmt.Errorf("expected several seeded backups still unexpired, found %d", len(rows))
	}
	// Not rows[0], which the floor keeps as the most recent, and not a verified one, which the floor
	// keeps as the last proof.
	for _, r := range rows[1:] {
		if r.verdict == "verified" {
			continue
		}
		return m.stampExpiry(ctx, r.id, 24*time.Hour)
	}
	return fmt.Errorf("every seeded backup of this instance is protected by the retention floor")
}
