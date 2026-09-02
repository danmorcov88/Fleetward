package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// optionBackupFilePattern names the connection option that overrides which files in the backup
	// directory count as backups. A glob, matched against the file name — for example "*.dump" or
	// "prod-*.sql.gz".
	optionBackupFilePattern = "backup_file_pattern"

	// historySourceDirectory is what a report says it read.
	historySourceDirectory = "a configured backup directory"

	// defaultHistoryLimit bounds a request that named no limit.
	defaultHistoryLimit = 500

	// maxHistoryLimit caps what a caller may ask for in one call.
	maxHistoryLimit = 5000
)

// defaultBackupFilePatterns are what a PostgreSQL backup written by somebody else's cron job is
// almost always called. They are a starting point rather than a claim: an estate that names its
// dumps something else sets backup_file_pattern on the connection.
var defaultBackupFilePatterns = []string{
	"*.dump", "*.dump.gz", "*.sql", "*.sql.gz", "*.sql.bz2", "*.sql.zst",
	"*.backup", "*.tar", "*.tar.gz", "*.pgdump", "*.custom",
}

// backupHistoryCapabilities is what this plugin promises about the evidence it reads, and most of
// it is a statement of what that evidence cannot do.
//
// PostgreSQL keeps no record of backups taken against it. pg_stat_archiver describes WAL archiving,
// which is a different question — it says nothing about whether last night's dump ran. What a
// PostgreSQL estate does usually have is a directory somebody's cron job writes into, and a
// directory can prove one thing: a file arrived, this big, at this time.
//
// That is worth having and it is worth being told plainly. A truncated pg_dump leaves a file behind
// exactly as a complete one does, so this source can never report success, and it says so here
// rather than letting anything downstream assume otherwise (ADR-0015).
func backupHistoryCapabilities() *fwv1.BackupHistoryCapabilities {
	return &fwv1.BackupHistoryCapabilities{
		Supported:         true,
		SourceDescription: historySourceDirectory,
		// A filesystem assigns nothing. The identity is derived from the path, which means a backup
		// renamed or moved is a different backup as far as this source can tell. Fleetward reports
		// that caveat rather than double counting silently.
		IdentityIsEngineAssigned: false,
		// The one thing this source fundamentally cannot do.
		ReportsOutcome: false,
		// It reads the directory of ADR-0026 — the same one an engine that hands its artifact over
		// as a file uses. Core refuses an observation schedule against an instance without one, at
		// the moment a human asks rather than at 02:00.
		RequiresSharedDirectory: true,
	}
}

// ListBackupHistory reports the backup files present in the instance's configured backup directory.
//
// It reads directory entries and nothing else: no file is opened, no byte is read, and nothing is
// written, moved, or deleted. The artifacts belong to whoever wrote them (ADR-0015).
func (p *Plugin) ListBackupHistory(ctx context.Context, req *fwv1.ListBackupHistoryRequest) (*fwv1.ListBackupHistoryResponse, error) {
	creds := req.GetCredentials()
	share := creds.GetSharedDirectory()
	if share.GetLocalPath() == "" {
		return nil, sdk.InvalidArgument(
			"this instance has no backup directory: PostgreSQL keeps no record of backups taken " +
				"against it, so observing them means reading the directory they are written to")
	}

	limit := int(req.GetLimit())
	switch {
	case limit <= 0:
		limit = defaultHistoryLimit
	case limit > maxHistoryLimit:
		limit = maxHistoryLimit
	}

	since := req.GetSince().AsTime()
	if token := req.GetPageToken(); token != "" {
		parsed, err := time.Parse(time.RFC3339Nano, token)
		if err != nil {
			return nil, sdk.InvalidArgument("the page token is not one this plugin issued")
		}
		since = parsed
	}

	patterns, err := filePatterns(creds.GetOptions()[optionBackupFilePattern])
	if err != nil {
		return nil, err
	}

	found, err := readBackupDirectory(ctx, share, patterns, since)
	if err != nil {
		return nil, err
	}

	// Oldest first, so a page boundary walks forward through time and the continuation token is
	// simply where it stopped.
	sort.Slice(found, func(i, j int) bool {
		if a, b := found[i].GetFinishedAt().AsTime(), found[j].GetFinishedAt().AsTime(); !a.Equal(b) {
			return a.Before(b)
		}
		return found[i].GetExternalId() < found[j].GetExternalId()
	})

	resp := &fwv1.ListBackupHistoryResponse{}
	if len(found) > limit {
		resp.Backups = found[:limit]
		resp.NextPageToken = resp.Backups[limit-1].GetFinishedAt().AsTime().Format(time.RFC3339Nano)
	} else {
		resp.Backups = found
	}
	return resp, nil
}

// readBackupDirectory turns directory entries into evidence.
//
// It reads one level. A layout that nests backups under per-database or per-day directories — what
// pgBackRest and Barman produce — is a different source with its own structure to understand, and
// guessing at one would be exactly the engine-shaped assumption the capability matrix exists to
// replace.
func readBackupDirectory(
	ctx context.Context,
	share *fwv1.SharedDirectory,
	patterns []string,
	since time.Time,
) ([]*fwv1.ObservedBackup, error) {
	entries, err := os.ReadDir(share.GetLocalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sdk.InvalidArgument(
				"the configured backup directory does not exist where Fleetward looks for it (%s)",
				share.GetLocalPath())
		}
		return nil, sdk.Internal("read the backup directory %s", share.GetLocalPath()).WithCause(err)
	}

	// The path the engine's own host knows the directory by, because that is the path a human will
	// go and look at. It falls back to the local one when only that is configured.
	base := share.GetEnginePath()
	if base == "" {
		base = share.GetLocalPath()
	}

	var out []*fwv1.ObservedBackup
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !matchesAny(patterns, entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// The file went away between the listing and the stat. Somebody else's retention just
			// ran; it is not a failure of this poll.
			continue
		}
		finished := info.ModTime().UTC()
		if !since.IsZero() && finished.Before(since) {
			continue
		}

		out = append(out, &fwv1.ObservedBackup{
			ExternalId: derivedIdentity(entry.Name()),
			// A file name is not a database name, and reading one out of the other would be a
			// guess presented as a fact.
			Database: "",
			Method:   "file",
			// Whether the file holds a dump or a physical copy is not knowable from outside it, and
			// this plugin does not open it.
			Kind: fwv1.BackupKind_BACKUP_KIND_UNSPECIFIED,
			// The only outcome a directory can support. See backupHistoryCapabilities.
			Outcome:    fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN,
			StartedAt:  nil,
			FinishedAt: timestamppb.New(finished),
			// A filesystem timestamp is already an absolute instant, so there is nothing here to
			// convert and nothing to be approximate about.
			FinishedAtIsApproximate: false,
			SizeBytes:               info.Size(),
			Location:                path.Join(base, entry.Name()),
			Details: map[string]string{
				"file_name": entry.Name(),
			},
		})
	}
	return out, nil
}

// derivedIdentity is how a file becomes a stable identity, and the choice is deliberate.
//
// It digests the file's name and nothing else — not its size, not its modification time. A backup
// written to the same path every night is then one record whose finish time moves, rather than a
// new record per night, and more importantly a file still being written while a poll runs updates
// one record instead of inserting a second one on the next poll. What it cannot survive is a
// rename, which is what identity_is_engine_assigned = false tells core to warn about.
func derivedIdentity(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "file:" + hex.EncodeToString(sum[:16])
}

// filePatterns resolves which names count as a backup.
func filePatterns(configured string) ([]string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return defaultBackupFilePatterns, nil
	}
	if _, err := filepath.Match(configured, "probe"); err != nil {
		return nil, sdk.InvalidArgument("%s is not a valid glob: %q", optionBackupFilePattern, configured)
	}
	return []string{configured}, nil
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		// The error case is impossible: every pattern was validated before it got here.
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}
