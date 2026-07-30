package inventory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// -----------------------------------------------------------------------------------------------
// Credential splitting
//
// These are the tests that matter most in this file. The split between the connections table and
// the secrets store is what keeps a password out of every read path, and nothing else enforces it.
// -----------------------------------------------------------------------------------------------

func TestConnectionOptionsNeverCarryTheSecret(t *testing.T) {
	const password = "s3cret-do-not-store-here"

	spec := &fwv1.ConnectionSpec{
		Username: "fleetward",
		Password: password,
		Database: "app",
		Options:  map[string]string{"sslmode": "require"},
		Tls: &fwv1.TLSSettings{
			Enabled:       true,
			ServerName:    "db.example.internal",
			CaPem:         []byte("-----BEGIN CERTIFICATE-----ca-----END CERTIFICATE-----"),
			ClientCertPem: []byte("-----BEGIN CERTIFICATE-----client-----END CERTIFICATE-----"),
			ClientKeyPem:  []byte("-----BEGIN PRIVATE KEY-----key-----END PRIVATE KEY-----"),
		},
	}

	optionsJSON, err := marshalConnectionOptions(spec)
	if err != nil {
		t.Fatalf("marshalConnectionOptions: %v", err)
	}

	if strings.Contains(string(optionsJSON), password) {
		t.Fatalf("the password reached the connections table: %s", optionsJSON)
	}
	// The private key is the other half of "can log in as this principal" and belongs in the secret
	// store with the password.
	if strings.Contains(string(optionsJSON), "PRIVATE KEY") {
		t.Fatalf("the client private key reached the connections table: %s", optionsJSON)
	}
	// The CA certificate is not secret and is deliberately kept where an operator can audit it.
	if !strings.Contains(string(optionsJSON), "ca") {
		t.Errorf("the CA certificate was not stored alongside the connection: %s", optionsJSON)
	}
}

func TestCredentialSecretRoundTrip(t *testing.T) {
	spec := &fwv1.ConnectionSpec{
		Username: "fleetward",
		Password: "correct horse battery staple",
		Options:  map[string]string{"sslmode": "verify-full"},
		Tls: &fwv1.TLSSettings{
			Enabled:            true,
			InsecureSkipVerify: true,
			ServerName:         "db.example.internal",
			CaPem:              []byte("ca"),
			ClientCertPem:      []byte("cert"),
			ClientKeyPem:       []byte("key"),
		},
	}

	payload, err := marshalCredentialSecret(spec)
	if err != nil {
		t.Fatalf("marshalCredentialSecret: %v", err)
	}
	optionsJSON, err := marshalConnectionOptions(spec)
	if err != nil {
		t.Fatalf("marshalConnectionOptions: %v", err)
	}

	var secret credentialSecret
	if err := json.Unmarshal(payload, &secret); err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	opts, err := unmarshalConnectionOptions(optionsJSON)
	if err != nil {
		t.Fatalf("unmarshalConnectionOptions: %v", err)
	}

	if secret.Password != spec.GetPassword() {
		t.Errorf("password did not survive the round trip")
	}
	if got := opts.Engine["sslmode"]; got != "verify-full" {
		t.Errorf("engine option sslmode = %q, want verify-full", got)
	}

	tls := opts.tlsSettings(true, &secret)
	if !tls.GetEnabled() {
		t.Error("TLS was not re-enabled")
	}
	if !tls.GetInsecureSkipVerify() {
		t.Error("insecure_skip_verify did not survive the round trip")
	}
	if tls.GetServerName() != "db.example.internal" {
		t.Errorf("server_name = %q", tls.GetServerName())
	}
	if string(tls.GetCaPem()) != "ca" || string(tls.GetClientCertPem()) != "cert" ||
		string(tls.GetClientKeyPem()) != "key" {
		t.Error("TLS material was not reassembled from both halves")
	}
}

// A nil TLS block is the contract's "no TLS". Sending an empty-but-present one would look to a
// plugin like a configuration that was lost on the way.
func TestTLSSettingsAreAbsentWhenDisabled(t *testing.T) {
	opts, err := unmarshalConnectionOptions(nil)
	if err != nil {
		t.Fatalf("unmarshalConnectionOptions: %v", err)
	}
	if got := opts.tlsSettings(false, &credentialSecret{}); got != nil {
		t.Errorf("tlsSettings = %v, want nil when TLS is disabled", got)
	}
}

// -----------------------------------------------------------------------------------------------
// Request validation
// -----------------------------------------------------------------------------------------------

func TestValidateCreateInstance(t *testing.T) {
	const validEnv = "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"

	valid := CreateInstanceInput{
		EnvironmentID: validEnv,
		Name:          "prod-1",
		EngineType:    "postgresql",
		Host:          "db.example.internal",
		Port:          5432,
		Connection:    &fwv1.ConnectionSpec{Username: "fleetward", Password: "p"},
	}

	// mutate applies a change to a copy of the valid input, so each case says only what is wrong.
	mutate := func(f func(*CreateInstanceInput)) CreateInstanceInput {
		in := valid
		f(&in)
		return in
	}

	tests := []struct {
		name    string
		input   CreateInstanceInput
		wantErr error
		// wantDetail is a fragment the message must contain, so a test cannot pass on the right
		// error for the wrong reason.
		wantDetail string
	}{
		{name: "valid", input: valid},
		{
			name:       "environment is required",
			input:      mutate(func(in *CreateInstanceInput) { in.EnvironmentID = "" }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "environment_id",
		},
		{
			name:       "environment must be a uuid",
			input:      mutate(func(in *CreateInstanceInput) { in.EnvironmentID = "production" }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "UUID",
		},
		{
			name:       "name is required",
			input:      mutate(func(in *CreateInstanceInput) { in.Name = "   " }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "name",
		},
		{
			name:       "host is required",
			input:      mutate(func(in *CreateInstanceInput) { in.Host = "" }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "host",
		},
		{
			// Core has no per-engine default port, and acquiring one would be exactly the engine
			// knowledge the plugin contract exists to keep out of core.
			name:       "port is required",
			input:      mutate(func(in *CreateInstanceInput) { in.Port = 0 }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "port",
		},
		{
			name:       "port must be in range",
			input:      mutate(func(in *CreateInstanceInput) { in.Port = 70000 }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "port",
		},
		{
			name:       "engine must have a plugin",
			input:      mutate(func(in *CreateInstanceInput) { in.EngineType = "informix" }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "no plugin serves",
		},
		{
			name:       "connection is required",
			input:      mutate(func(in *CreateInstanceInput) { in.Connection = nil }),
			wantErr:    ErrInvalidArgument,
			wantDetail: "connection",
		},
		{
			name: "username is required",
			input: mutate(func(in *CreateInstanceInput) {
				in.Connection = &fwv1.ConnectionSpec{Password: "p"}
			}),
			wantErr:    ErrInvalidArgument,
			wantDetail: "username",
		},
	}

	svc := &Service{plugins: &fakeRouter{engines: []string{"postgresql"}}, tenantID: "tenant"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, _, err := svc.validateCreateInstance(tt.input)

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if inst.GetEngineType() != "postgresql" {
					t.Errorf("engine_type = %q", inst.GetEngineType())
				}
				return
			}

			if err == nil {
				t.Fatalf("expected an error")
			}
			if !isSentinel(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantDetail) {
				t.Errorf("message %q does not mention %q", err, tt.wantDetail)
			}
		})
	}
}

// The engine type is a routing key, so it is normalized once here rather than at every comparison.
func TestValidateCreateInstanceNormalizesInput(t *testing.T) {
	svc := &Service{plugins: &fakeRouter{engines: []string{"postgresql"}}, tenantID: "tenant"}

	inst, _, err := svc.validateCreateInstance(CreateInstanceInput{
		EnvironmentID: "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f",
		Name:          "  prod-1  ",
		EngineType:    "  PostgreSQL  ",
		Host:          "  db.example.internal  ",
		Port:          5432,
		Connection:    &fwv1.ConnectionSpec{Username: "fleetward"},
	})
	if err != nil {
		t.Fatalf("validateCreateInstance: %v", err)
	}
	if inst.GetName() != "prod-1" {
		t.Errorf("name = %q", inst.GetName())
	}
	if inst.GetEngineType() != "postgresql" {
		t.Errorf("engine_type = %q", inst.GetEngineType())
	}
	if inst.GetHost() != "db.example.internal" {
		t.Errorf("host = %q", inst.GetHost())
	}
	if inst.GetHealth() != fwv1.HealthState_HEALTH_STATE_UNKNOWN {
		t.Errorf("health = %v; a new instance has not been probed yet", inst.GetHealth())
	}
}

// -----------------------------------------------------------------------------------------------
// Identifiers, cursors, and paging
// -----------------------------------------------------------------------------------------------

func TestIsUUID(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f", true},
		{"0F2D1C3E-4A5B-4C7D-8E9F-0A1B2C3D4E5F", true},
		{"", false},
		{"prod-1", false},
		{"0f2d1c3e4a5b4c7d8e9f0a1b2c3d4e5f", false},
		{"0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5", false},
		{"0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5g", false},
		{"0f2d1c3e_4a5b_4c7d_8e9f_0a1b2c3d4e5f", false},
		// A quoted identifier is how SQL injection would arrive; it must not reach a query.
		{"' OR 1=1 --                          ", false},
	}
	for _, tt := range tests {
		if got := isUUID(tt.value); got != tt.want {
			t.Errorf("isUUID(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	const id = "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"
	// A nanosecond component that a second-resolution format would silently drop, which would make
	// two rows created in the same second paginate incorrectly.
	created := time.Date(2026, 7, 28, 9, 30, 15, 123456789, time.UTC)

	got, err := decodeCursor(encodeCursor(created, id))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !got.set {
		t.Fatal("cursor is not marked as set")
	}
	if !got.createdAt.Equal(created) {
		t.Errorf("created_at = %s, want %s", got.createdAt, created)
	}
	if got.id != id {
		t.Errorf("id = %q, want %q", got.id, id)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, token := range []string{
		"not-base64-%%%",
		"bm8tc2VwYXJhdG9y", // "no-separator"
		"MjAyNi0wNy0yOFQwOTozMDoxNVp8bm90LWEtdXVpZA",                      // valid time, invalid id
		"bm90LWEtdGltZXwwZjJkMWMzZS00YTViLTRjN2QtOGU5Zi0wYTFiMmMzZDRlNWY", // invalid time
	} {
		if _, err := decodeCursor(token); !isSentinel(err, ErrInvalidArgument) {
			t.Errorf("decodeCursor(%q) error = %v, want ErrInvalidArgument", token, err)
		}
	}
}

func TestDecodeEmptyCursorStartsAtTheBeginning(t *testing.T) {
	got, err := decodeCursor("")
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if got.after() != nil || got.afterID() != nil {
		t.Error("an empty token must produce NULL bounds so the first page is unfiltered")
	}
}

func TestClampPageSize(t *testing.T) {
	tests := []struct {
		requested int32
		want      int32
		wantErr   bool
	}{
		{requested: 0, want: defaultPageSize},
		{requested: 10, want: 10},
		{requested: maxPageSize, want: maxPageSize},
		// Capped rather than rejected: a client asking for everything gets a page and a token.
		{requested: 100000, want: maxPageSize},
		{requested: -1, wantErr: true},
	}
	for _, tt := range tests {
		got, err := clampPageSize(tt.requested)
		if tt.wantErr {
			if !isSentinel(err, ErrInvalidArgument) {
				t.Errorf("clampPageSize(%d) error = %v, want ErrInvalidArgument", tt.requested, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("clampPageSize(%d): %v", tt.requested, err)
		}
		if got != tt.want {
			t.Errorf("clampPageSize(%d) = %d, want %d", tt.requested, got, tt.want)
		}
	}
}

func TestNullableUUID(t *testing.T) {
	if got, err := nullableUUID("environment_id", ""); got != nil || err != nil {
		t.Errorf("an empty filter must mean NULL, got %v, %v", got, err)
	}
	if _, err := nullableUUID("environment_id", "production"); !isSentinel(err, ErrInvalidArgument) {
		t.Errorf("error = %v, want ErrInvalidArgument", err)
	}
	got, err := nullableUUID("environment_id", "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f")
	if err != nil || got == nil || *got != "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f" {
		t.Errorf("got %v, %v", got, err)
	}
}

// -----------------------------------------------------------------------------------------------
// Stored health
// -----------------------------------------------------------------------------------------------

func TestParseHealthState(t *testing.T) {
	tests := []struct {
		stored string
		want   fwv1.HealthState
	}{
		{"HEALTH_STATE_UP", fwv1.HealthState_HEALTH_STATE_UP},
		{"HEALTH_STATE_DEGRADED", fwv1.HealthState_HEALTH_STATE_DEGRADED},
		{"HEALTH_STATE_DOWN", fwv1.HealthState_HEALTH_STATE_DOWN},
		{"HEALTH_STATE_UNKNOWN", fwv1.HealthState_HEALTH_STATE_UNKNOWN},
		// A value written by a newer version, or edited by hand. A row nobody can read would be
		// worse than one whose health is uncertain.
		{"HEALTH_STATE_SOMETHING_NEW", fwv1.HealthState_HEALTH_STATE_UNKNOWN},
		{"", fwv1.HealthState_HEALTH_STATE_UNKNOWN},
	}
	for _, tt := range tests {
		if got := parseHealthState(tt.stored); got != tt.want {
			t.Errorf("parseHealthState(%q) = %v, want %v", tt.stored, got, tt.want)
		}
	}
}

func TestLabelsOrEmptyNeverProducesNull(t *testing.T) {
	// instances.labels is NOT NULL; a nil map would be written as SQL NULL and fail the constraint.
	if got := labelsOrEmpty(nil); got == nil || len(got) != 0 {
		t.Errorf("labelsOrEmpty(nil) = %v, want an empty map", got)
	}
	in := map[string]string{"team": "platform"}
	if got := labelsOrEmpty(in); got["team"] != "platform" {
		t.Errorf("labelsOrEmpty dropped a label: %v", got)
	}
}

// -----------------------------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------------------------

// fakeRouter stands in for the plugin manager so these tests need no plugin processes.
type fakeRouter struct {
	engines []string
	client  fwv1.EnginePluginClient
}

func (f *fakeRouter) Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error) {
	caps, err := f.Capabilities(engineType)
	if err != nil {
		return nil, nil, err
	}
	return f.client, caps, nil
}

func (f *fakeRouter) Capabilities(engineType string) (*fwv1.Capabilities, error) {
	for _, known := range f.engines {
		if known == engineType {
			return &fwv1.Capabilities{EngineType: engineType}, nil
		}
	}
	return nil, errNoSuchEngine
}

func (f *fakeRouter) EngineTypes() []string { return f.engines }

var errNoSuchEngine = errors.New("no plugin available for engine type")

// isSentinel reports whether err carries the expected sentinel.
func isSentinel(err, target error) bool { return errors.Is(err, target) }
