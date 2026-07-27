package sdk

import (
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// minimal returns the smallest capability matrix that is internally coherent: a plugin that has
// handshaked but implements nothing yet.
func minimal() *fwv1.Capabilities {
	return &fwv1.Capabilities{
		EngineType:      "testengine",
		PluginVersion:   "0.1.0",
		ContractVersion: "v1",
	}
}

func withSandbox(c *fwv1.Capabilities) *fwv1.Capabilities {
	c.SupportsSandboxRestore = true
	c.SandboxTemplate = &fwv1.SandboxTemplate{
		ImageRepository: "testengine",
		DefaultTag:      "1",
		ContainerPort:   5432,
	}
	return c
}

func TestValidateCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		caps       *fwv1.Capabilities
		wantErr    bool
		wantErrHas string
	}{
		{
			name: "minimal stage-0 plugin is valid",
			caps: minimal(),
		},
		{
			name:    "nil",
			caps:    nil,
			wantErr: true,
		},
		{
			name: "missing engine type",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.EngineType = ""
				return c
			}(),
			wantErr:    true,
			wantErrHas: "engine_type",
		},
		{
			name: "missing plugin version",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.PluginVersion = ""
				return c
			}(),
			wantErr:    true,
			wantErrHas: "plugin_version",
		},
		{
			name: "one default backup method is valid",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true},
					{Id: "physical", Kind: fwv1.BackupKind_BACKUP_KIND_PHYSICAL},
				}
				return c
			}(),
		},
		{
			name: "no default backup method",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL},
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "is_default",
		},
		{
			name: "two default backup methods",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "a", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true},
					{Id: "b", Kind: fwv1.BackupKind_BACKUP_KIND_PHYSICAL, IsDefault: true},
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "is_default",
		},
		{
			name: "duplicate method id",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true},
					{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_PHYSICAL},
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "duplicate id",
		},
		{
			name: "method missing kind",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{{Id: "dump", IsDefault: true}}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "kind is required",
		},
		{
			name: "pitr without a baseline-capable method",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportsPitr = true
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true},
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "enables_pitr",
		},
		{
			name: "pitr with a baseline-capable method",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportsPitr = true
				c.BackupMethods = []*fwv1.BackupMethod{
					{Id: "base", Kind: fwv1.BackupKind_BACKUP_KIND_PHYSICAL, IsDefault: true, EnablesPitr: true},
				}
				return c
			}(),
		},
		{
			name: "point-in-time restore without pitr",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportsPointInTimeRestore = true
				return c
			}(),
			wantErr:    true,
			wantErrHas: "supports_pitr",
		},
		{
			name: "verification checks without sandbox restore",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportedVerificationChecks = []fwv1.VerificationCheck{
					fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "supports_sandbox_restore",
		},
		{
			name: "sandbox restore without a template",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportsSandboxRestore = true
				return c
			}(),
			wantErr:    true,
			wantErrHas: "sandbox_template",
		},
		{
			name: "sandbox restore with a complete template",
			caps: func() *fwv1.Capabilities {
				c := withSandbox(minimal())
				c.SupportedVerificationChecks = []fwv1.VerificationCheck{
					fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
					fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
				}
				return c
			}(),
		},
		{
			name: "replication lag without replication",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.SupportsReplicationLag = true
				return c
			}(),
			wantErr:    true,
			wantErrHas: "supports_replication",
		},
		{
			name: "enum option without allowed values",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.BackupMethods = []*fwv1.BackupMethod{{
					Id:        "dump",
					Kind:      fwv1.BackupKind_BACKUP_KIND_LOGICAL,
					IsDefault: true,
					Options: []*fwv1.MethodOption{
						{Name: "format", Type: fwv1.OptionType_OPTION_TYPE_ENUM},
					},
				}}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "allowed_values",
		},
		{
			name: "duplicate metric name",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.Metrics = []*fwv1.MetricDescriptor{
					{Name: "db.client.connection.count", Type: fwv1.MetricType_METRIC_TYPE_GAUGE},
					{Name: "db.client.connection.count", Type: fwv1.MetricType_METRIC_TYPE_GAUGE},
				}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "duplicate name",
		},
		{
			name: "metric missing type",
			caps: func() *fwv1.Capabilities {
				c := minimal()
				c.Metrics = []*fwv1.MetricDescriptor{{Name: "db.client.connection.count"}}
				return c
			}(),
			wantErr:    true,
			wantErrHas: "type is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCapabilities(tc.caps)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCapabilities() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErrHas != "" && !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Errorf("error %q does not mention %q", err, tc.wantErrHas)
			}
		})
	}
}

func TestDefaultBackupMethod(t *testing.T) {
	tests := []struct {
		name    string
		methods []*fwv1.BackupMethod
		want    string
	}{
		{"none", nil, ""},
		{"explicit default", []*fwv1.BackupMethod{
			{Id: "a"}, {Id: "b", IsDefault: true},
		}, "b"},
		{"falls back to the first", []*fwv1.BackupMethod{
			{Id: "a"}, {Id: "b"},
		}, "a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultBackupMethod(&fwv1.Capabilities{BackupMethods: tc.methods})
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %q, want nil", got.GetId())
				}
				return
			}
			if got.GetId() != tc.want {
				t.Fatalf("got %q, want %q", got.GetId(), tc.want)
			}
		})
	}
}

func TestFindBackupMethod(t *testing.T) {
	caps := &fwv1.Capabilities{BackupMethods: []*fwv1.BackupMethod{{Id: "dump"}, {Id: "physical"}}}
	if got := FindBackupMethod(caps, "physical"); got.GetId() != "physical" {
		t.Errorf("got %q, want %q", got.GetId(), "physical")
	}
	if got := FindBackupMethod(caps, "absent"); got != nil {
		t.Errorf("got %q, want nil", got.GetId())
	}
}

func TestSupportsCheck(t *testing.T) {
	caps := &fwv1.Capabilities{
		SupportedVerificationChecks: []fwv1.VerificationCheck{
			fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		},
	}
	if !SupportsCheck(caps, fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY) {
		t.Error("connectivity check should be reported as supported")
	}
	if SupportsCheck(caps, fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY) {
		t.Error("integrity check should not be reported as supported")
	}
}
