package backup

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

func capsWith(methods ...*fwv1.BackupMethod) *fwv1.Capabilities {
	return &fwv1.Capabilities{
		EngineType:    "testengine",
		PluginVersion: "0.1.0",
		BackupMethods: methods,
	}
}

func TestSelectMethod(t *testing.T) {
	dump := &fwv1.BackupMethod{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true}
	physical := &fwv1.BackupMethod{Id: "physical", Kind: fwv1.BackupKind_BACKUP_KIND_PHYSICAL}

	tests := []struct {
		name      string
		caps      *fwv1.Capabilities
		requested string
		wantID    string
		wantErr   error
	}{
		{
			name:      "empty picks the plugin's default",
			caps:      capsWith(dump, physical),
			requested: "",
			wantID:    "dump",
		},
		{
			name:      "a named method is honored",
			caps:      capsWith(dump, physical),
			requested: "physical",
			wantID:    "physical",
		},
		{
			name:      "an unknown method is a bad request, not a failure at 3am",
			caps:      capsWith(dump, physical),
			requested: "pgbackrest",
			wantErr:   ErrInvalidArgument,
		},
		{
			// Core reads the capability matrix and nothing else. A plugin with no method is an
			// engine Fleetward cannot back up, which is different from a backup that failed.
			name:    "a plugin with no method is unsupported",
			caps:    capsWith(),
			wantErr: ErrUnsupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method, err := selectMethod(tc.caps, tc.requested)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("selectMethod() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectMethod() error = %v", err)
			}
			if method.GetId() != tc.wantID {
				t.Errorf("selectMethod() = %q, want %q", method.GetId(), tc.wantID)
			}
		})
	}
}

func TestValidateOptions(t *testing.T) {
	method := &fwv1.BackupMethod{
		Id:   "dump",
		Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL,
		Options: []*fwv1.MethodOption{
			{
				Name:          "format",
				Type:          fwv1.OptionType_OPTION_TYPE_ENUM,
				AllowedValues: []string{"custom", "plain"},
			},
			{Name: "jobs", Type: fwv1.OptionType_OPTION_TYPE_INT},
		},
	}

	tests := []struct {
		name    string
		options map[string]string
		wantErr bool
	}{
		{name: "none", options: nil},
		{name: "a declared enum value", options: map[string]string{"format": "plain"}},
		{name: "a declared non-enum option", options: map[string]string{"jobs": "4"}},
		{
			// A misspelled option that is silently ignored produces a backup taken with settings
			// nobody chose, which is worse than a rejected request.
			name:    "an option the method does not declare",
			options: map[string]string{"fomrat": "plain"},
			wantErr: true,
		},
		{
			name:    "a value outside the declared enum",
			options: map[string]string{"format": "directory"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptions(method, tc.options)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateOptions() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("error %v should classify as ErrInvalidArgument", err)
			}
		})
	}
}

// TestClassifyPluginError pins the distinction core acts on: an engine that cannot do something is
// not the same as one that tried and failed.
func TestClassifyPluginError(t *testing.T) {
	unsupported := classifyPluginError(&fwv1.PluginError{Code: fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED})
	if !errors.Is(unsupported, ErrUnsupported) {
		t.Errorf("UNSUPPORTED classified as %v", unsupported)
	}
	failed := classifyPluginError(&fwv1.PluginError{Code: fwv1.ErrorCode_ERROR_CODE_TOOL_FAILED})
	if !errors.Is(failed, ErrPluginFailed) {
		t.Errorf("TOOL_FAILED classified as %v", failed)
	}
}

func TestRequireUUID(t *testing.T) {
	valid := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	if got, err := requireUUID("backup_id", valid); err != nil || got != valid {
		t.Fatalf("requireUUID(%q) = %q, %v", valid, got, err)
	}

	for _, invalid := range []string{"", "not-a-uuid", "3f2504e0-4f89-11d3-9a0c-0305e82c330", "../../etc/passwd"} {
		if _, err := requireUUID("backup_id", invalid); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("requireUUID(%q) error = %v, want ErrInvalidArgument", invalid, err)
		}
	}
}

func TestParseBackupState(t *testing.T) {
	for name, want := range map[string]fwv1.BackupState{
		"pending":   fwv1.BackupState_BACKUP_STATE_PENDING,
		"running":   fwv1.BackupState_BACKUP_STATE_RUNNING,
		"succeeded": fwv1.BackupState_BACKUP_STATE_SUCCEEDED,
		"failed":    fwv1.BackupState_BACKUP_STATE_FAILED,
		"canceled":  fwv1.BackupState_BACKUP_STATE_CANCELED,
		"expired":   fwv1.BackupState_BACKUP_STATE_EXPIRED,
		"invented":  fwv1.BackupState_BACKUP_STATE_UNSPECIFIED,
	} {
		if got := parseBackupState(name); got != want {
			t.Errorf("parseBackupState(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestArtifactKeyIsTenantScoped guards the prefix layout a bucket policy or lifecycle rule is
// scoped by. Changing it silently would make one tenant's rules stop matching their objects.
func TestArtifactKeyIsTenantScoped(t *testing.T) {
	key := artifactKeyFor("tenant-1", "instance-2", "backup-3")
	want := "tenants/tenant-1/instances/instance-2/backups/backup-3/" + artifactFilename
	if key != want {
		t.Errorf("artifact key = %q, want %q", key, want)
	}
	if !strings.HasPrefix(key, "tenants/tenant-1/") {
		t.Error("the key must start with the tenant prefix")
	}
}

// discardLogger builds a logger for tests that exercise code paths which log.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
