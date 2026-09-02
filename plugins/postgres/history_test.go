package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// TestDerivedIdentityIsStable is the property that keeps a poll from filling the metadata database
// with copies of one nightly backup.
//
// The identity digests the file name and nothing else — not its size, not its modification time —
// so a dump still being written while a poll runs updates one record on the next poll instead of
// inserting a second one. What it deliberately cannot survive is a rename, which is exactly what
// identity_is_engine_assigned = false tells core to warn about (ADR-0027).
func TestDerivedIdentityIsStable(t *testing.T) {
	first := derivedIdentity("nightly.dump")
	if first == "" {
		t.Fatal("derivedIdentity returned an empty identity")
	}
	if again := derivedIdentity("nightly.dump"); again != first {
		t.Errorf("the same file produced two identities: %q then %q", first, again)
	}
	if other := derivedIdentity("nightly-2.dump"); other == first {
		t.Error("two different files share one identity, so one would overwrite the other")
	}
}

func TestFilePatterns(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		match      []string
		reject     []string
		wantErr    bool
	}{
		{
			name:   "the defaults cover what a cron job usually writes",
			match:  []string{"nightly.dump", "prod.sql.gz", "app.backup", "cluster.tar.gz"},
			reject: []string{"README", "nightly.log", "pg_wal"},
		},
		{
			name:       "a configured glob replaces them entirely",
			configured: "prod-*.dump",
			match:      []string{"prod-2026-09-02.dump"},
			// Narrowing means narrowing: an estate that says which files are backups is not asking
			// for the defaults as well.
			reject: []string{"staging-2026-09-02.dump", "prod.sql"},
		},
		{
			name:       "a glob that does not compile is refused rather than silently matching nothing",
			configured: "prod-[.dump",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patterns, err := filePatterns(tt.configured)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("filePatterns: %v", err)
			}
			for _, name := range tt.match {
				if !matchesAny(patterns, name) {
					t.Errorf("%q was not recognised as a backup", name)
				}
			}
			for _, name := range tt.reject {
				if matchesAny(patterns, name) {
					t.Errorf("%q was recognised as a backup", name)
				}
			}
		})
	}
}

// TestReadBackupDirectory exercises the whole of what this source can prove, against a real
// directory. It needs no database: PostgreSQL keeps no record of its own backups, which is the
// finding this implementation is built around.
func TestReadBackupDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("set the time on %s: %v", name, err)
		}
		return path
	}

	write("last-night.dump", 4096, time.Hour)
	write("a-week-ago.dump", 2048, 7*24*time.Hour)
	write("notes.txt", 10, time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0o750); err != nil {
		t.Fatalf("create a subdirectory: %v", err)
	}

	share := &fwv1.SharedDirectory{EnginePath: "/srv/backups", LocalPath: dir}
	patterns, err := filePatterns("")
	if err != nil {
		t.Fatalf("filePatterns: %v", err)
	}

	all, err := readBackupDirectory(context.Background(), share, patterns, time.Time{})
	if err != nil {
		t.Fatalf("readBackupDirectory: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("found %d backups, want 2 — a text file and a directory are not backups", len(all))
	}

	byName := map[string]*fwv1.ObservedBackup{}
	for _, b := range all {
		byName[b.GetDetails()["file_name"]] = b
	}

	got, ok := byName["last-night.dump"]
	if !ok {
		t.Fatal("last-night.dump was not reported")
	}
	if got.GetSizeBytes() != 4096 {
		t.Errorf("size = %d, want 4096", got.GetSizeBytes())
	}
	// The whole point of this source's honesty: a truncated dump leaves a file behind exactly as a
	// complete one does, so success is not something a directory can report.
	if got.GetOutcome() != fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN {
		t.Errorf("outcome = %s, want UNKNOWN from a directory listing", got.GetOutcome())
	}
	if got.GetFinishedAtIsApproximate() {
		t.Error("a filesystem timestamp is an absolute instant; nothing about it is approximate")
	}
	// The location is the path a human would go and look at, which is the engine host's, not the
	// path this control plane happens to reach the same directory by.
	if want := "/srv/backups/last-night.dump"; got.GetLocation() != want {
		t.Errorf("location = %q, want %q", got.GetLocation(), want)
	}

	// `since` is a filter core relies on to keep a poll incremental.
	recent, err := readBackupDirectory(context.Background(), share, patterns, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("readBackupDirectory: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("found %d backups in the last day, want 1", len(recent))
	}
}

func TestListBackupHistoryRefusesAnInstanceWithNoDirectory(t *testing.T) {
	p := New()
	_, err := p.ListBackupHistory(context.Background(), &fwv1.ListBackupHistoryRequest{
		Credentials: &fwv1.Credentials{Host: "127.0.0.1", Port: 5432},
	})
	if err == nil {
		t.Fatal("want a refusal for an instance with no backup directory, got none")
	}
}

func TestListBackupHistoryRejectsAPageTokenItDidNotIssue(t *testing.T) {
	p := New()
	dir := t.TempDir()
	_, err := p.ListBackupHistory(context.Background(), &fwv1.ListBackupHistoryRequest{
		Credentials: &fwv1.Credentials{
			Host: "127.0.0.1", Port: 5432,
			SharedDirectory: &fwv1.SharedDirectory{EnginePath: dir, LocalPath: dir},
		},
		Since:     timestamppb.New(time.Now().Add(-time.Hour)),
		PageToken: "not a timestamp",
	})
	if err == nil {
		t.Fatal("want a refusal for an unparseable page token, got none")
	}
}

// TestBackupHistoryCapabilitiesAreHonest guards the one thing that would make this feature worse
// than useless: a plugin overstating what its evidence can prove.
func TestBackupHistoryCapabilitiesAreHonest(t *testing.T) {
	caps := backupHistoryCapabilities()
	if !caps.GetSupported() {
		t.Fatal("this plugin implements ListBackupHistory and must declare it")
	}
	if caps.GetSourceDescription() == "" {
		t.Error("a report has to be able to say what it read")
	}
	if caps.GetReportsOutcome() {
		t.Error("a directory listing cannot tell a complete dump from a truncated one")
	}
	if caps.GetIdentityIsEngineAssigned() {
		t.Error("a filesystem assigns no identity; a renamed file is a new file")
	}
	if !caps.GetRequiresSharedDirectory() {
		t.Error("there is nothing to read without the directory the backups are written to")
	}
}
