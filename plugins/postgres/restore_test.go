package postgres

import (
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// TestClassifyRestoreDiagnostics is the difference between an alert that gets read and one that
// gets muted. pg_restore exits non-zero for reasons that say nothing about the data, and reporting
// every one of them as a failed verification would fire the critical alert on every healthy restore.
func TestClassifyRestoreDiagnostics(t *testing.T) {
	tests := []struct {
		name            string
		stderr          string
		wantFatal       int
		wantCosmetic    int
		wantUnreachable int
	}{
		{
			name:   "clean run",
			stderr: "",
		},
		{
			// The sandbox has no copy of the source cluster's roles, and never will.
			name:         "a missing role is cosmetic",
			stderr:       `pg_restore: error: could not execute query: ERROR:  role "app_owner" does not exist`,
			wantCosmetic: 1,
		},
		{
			name:         "an object the template database already provides is cosmetic",
			stderr:       `pg_restore: error: could not execute query: ERROR:  schema "public" already exists`,
			wantCosmetic: 1,
		},
		{
			name:         "a comment only a superuser may set is cosmetic",
			stderr:       `pg_restore: error: could not execute query: ERROR:  must be owner of extension plpgsql`,
			wantCosmetic: 1,
		},
		{
			// The one that matters: the archive itself is unreadable.
			name:      "a corrupt archive is fatal",
			stderr:    "pg_restore: error: could not read from input file: end of file",
			wantFatal: 1,
		},
		{
			// A client newer than the server opens the restore by setting a parameter the server
			// has never heard of. Failing over it would mean no backup from an older server could
			// ever be verified.
			name:         "a parameter the older sandbox does not know is cosmetic",
			stderr:       `pg_restore: error: could not execute query: ERROR:  unrecognized configuration parameter "transaction_timeout"`,
			wantCosmetic: 1,
		},
		{
			name:      "a missing extension is fatal",
			stderr:    `pg_restore: error: could not execute query: ERROR:  could not open extension control file "/usr/share/postgresql/16/extension/postgis.control"`,
			wantFatal: 1,
		},
		{
			name: "the closing tally is not counted twice",
			stderr: `pg_restore: error: could not execute query: ERROR:  role "app_owner" does not exist` + "\n" +
				"pg_restore: warning: errors ignored on restore: 1",
			wantCosmetic: 1,
		},
		{
			name: "a mixture keeps only the real failure fatal",
			stderr: `pg_restore: error: could not execute query: ERROR:  role "app_owner" does not exist` + "\n" +
				"pg_restore: error: could not read from input file: end of file",
			wantFatal:    1,
			wantCosmetic: 1,
		},
		{
			// Progress output must not be mistaken for a failure.
			name:   "informational output is ignored",
			stderr: "pg_restore: connecting to database for restore\npg_restore: creating TABLE \"public.customers\"",
		},
		{
			// The sandbox never answered. This is infrastructure trouble, and calling it fatal
			// would report a perfectly good backup as data loss (ADR-0022).
			name: "a refused connection is not evidence about the artifact",
			stderr: "pg_restore: error: connection to server at \"127.0.0.1\", port 32770 failed: " +
				"Connection refused",
			wantUnreachable: 1,
		},
		{
			// A sandbox that died mid-restore leaves wreckage on stderr after the real cause. The
			// first line is what happened; the rest must not promote the run to a data failure.
			name: "a connection lost mid-restore outranks the wreckage after it",
			stderr: "pg_restore: error: server closed the connection unexpectedly\n" +
				"pg_restore: error: could not execute query: ERROR:  relation \"customers\" does not exist",
			wantFatal:       1,
			wantUnreachable: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fatal, cosmetic, unreachable := classifyRestoreDiagnostics(tc.stderr)
			if len(fatal) != tc.wantFatal {
				t.Errorf("fatal = %d %v, want %d", len(fatal), fatal, tc.wantFatal)
			}
			if len(cosmetic) != tc.wantCosmetic {
				t.Errorf("cosmetic = %d %v, want %d", len(cosmetic), cosmetic, tc.wantCosmetic)
			}
			if len(unreachable) != tc.wantUnreachable {
				t.Errorf("unreachable = %d %v, want %d", len(unreachable), unreachable, tc.wantUnreachable)
			}
		})
	}
}

// TestRestoreArgsNeverCarryThePassword is the same guard the dump path has. Everything on argv is
// visible through ps to every user on the host, and this process is holding credentials for a
// database that contains a full copy of production.
func TestRestoreArgsNeverCarryThePassword(t *testing.T) {
	const password = "s3cr3t-sandbox-password"
	creds := &fwv1.Credentials{
		Host: "127.0.0.1", Port: 55432, Username: "fleetward",
		Password: password, Database: "fleetward_sandbox",
	}

	for _, tool := range []string{restoreTool, psqlTool} {
		t.Run(tool, func(t *testing.T) {
			args := restoreArgs(restoreOptions{
				tool: tool, creds: creds, database: creds.GetDatabase(), file: "/tmp/artifact",
			})
			joined := strings.Join(args, " ")
			if strings.Contains(joined, password) {
				t.Fatalf("the password appears on argv: %s", joined)
			}
			if !strings.Contains(joined, "--no-password") {
				t.Error("--no-password is absent; a missing password would prompt on a terminal nobody is watching")
			}
			if !strings.Contains(joined, "--dbname=fleetward_sandbox") {
				t.Error("the target database is not named on the command line")
			}
		})
	}
}

// TestPgRestoreSkipsOwnershipAndPrivileges records why the flags are there: the roles a dump refers
// to exist on the source cluster and nowhere else, so restoring them would fail on every object for
// a reason that says nothing about whether the data survived.
func TestPgRestoreSkipsOwnershipAndPrivileges(t *testing.T) {
	args := restoreArgs(restoreOptions{
		tool:  restoreTool,
		creds: &fwv1.Credentials{Host: "127.0.0.1", Username: "fleetward"},
		file:  "/tmp/artifact",
	})

	joined := strings.Join(args, " ")
	for _, flag := range []string{"--no-owner", "--no-privileges", "--no-comments"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("%s is absent from %s", flag, joined)
		}
	}
	// The archive is the last argument, which is what pg_restore expects.
	if args[len(args)-1] != "/tmp/artifact" {
		t.Errorf("last argument = %q, want the archive path", args[len(args)-1])
	}
}

// TestRestoreRefusesARealInstance keeps the capability matrix and the behaviour from drifting apart
// while restoring over a live database is still out of scope. A plugin willing to do it before core
// can authorize it is one accident away from an outage.
func TestRestoreRefusesARealInstance(t *testing.T) {
	_, err := restoreTargetCredentials(&fwv1.RestoreTarget{
		Kind:        fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_INSTANCE,
		Credentials: &fwv1.Credentials{Host: "prod.example.internal"},
	})
	if err == nil {
		t.Fatal("a restore over a real instance was accepted")
	}
	if code := sdk.AsPluginError(err).GetCode(); code != fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
		t.Errorf("code = %v, want UNSUPPORTED so core reports it as unavailable rather than broken", code)
	}
}

// TestRestoreEnvironmentDropsInheritedPGVariables repeats the dump path's rule for the restore
// child: the control plane's own environment may carry PGDATABASE or PGSSLMODE for unrelated
// reasons, and inheriting one would silently redirect a restore.
func TestRestoreEnvironmentDropsInheritedPGVariables(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "PGDATABASE=someone_elses_database", "PGSSLMODE=disable", "HOME=/root"}

	env, err := dumpEnv(parent, &fwv1.Credentials{
		Host: "127.0.0.1", Username: "fleetward", Password: "sandbox-password",
	}, tlsMaterial{})
	if err != nil {
		t.Fatalf("dumpEnv: %v", err)
	}

	for _, entry := range env {
		if strings.HasPrefix(entry, "PGDATABASE=someone_elses_database") {
			t.Error("an inherited PGDATABASE survived and would redirect the restore")
		}
	}
	if !containsPrefix(env, "PGPASSWORD=") {
		t.Error("the password does not travel in the environment, so the restore would prompt")
	}
}

func containsPrefix(entries []string, prefix string) bool {
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
