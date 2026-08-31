package backup

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// TestClassifyRestoreOutcome is the distinction this slice's whole failure model rests on.
//
// FAILED means the backup is bad and must wake somebody. INCONCLUSIVE means we could not tell, and
// must not. Collapsing the two in either direction is a product defect rather than a code one: a
// failure that is reported as inconclusive hides data loss, and an infrastructure problem reported
// as a failure trains an operator to ignore the one alert that matters most.
func TestClassifyRestoreOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want fwv1.VerificationStatus
	}{
		{
			name: "an artifact that fails its checksum is data loss",
			err: &restoreError{plugin: sdk.ArtifactCorrupt(
				"the artifact does not match its checksum").Proto()},
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED,
		},
		{
			name: "the engine's own tooling refusing the artifact is data loss",
			err: &restoreError{plugin: &fwv1.PluginError{
				Code:    fwv1.ErrorCode_ERROR_CODE_TOOL_FAILED,
				Message: "pg_restore: error: could not read from input file: end of file",
			}},
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED,
		},
		{
			// The plugin's host has no pg_restore. That says nothing about the backup.
			name: "a missing tool is an infrastructure problem",
			err: &restoreError{plugin: &fwv1.PluginError{
				Code:    fwv1.ErrorCode_ERROR_CODE_TOOL_NOT_FOUND,
				Message: `required tool "pg_restore" was not found on PATH`,
			}},
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		},
		{
			name: "a transfer that broke is an infrastructure problem",
			err: &restoreError{plugin: &fwv1.PluginError{
				Code:    fwv1.ErrorCode_ERROR_CODE_OBJECT_STORE_FAILED,
				Message: "download the artifact: connection reset",
			}},
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		},
		{
			name: "an unreachable sandbox is an infrastructure problem",
			err: &restoreError{plugin: &fwv1.PluginError{
				Code:    fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
				Message: "cannot reach 127.0.0.1:55432",
			}},
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		},
		{
			name: "a stream that died without saying why is inconclusive",
			err:  errors.New("the plugin ended the restore stream without reporting an outcome"),
			want: fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRestoreOutcome(tc.err)
			if got.status != tc.want {
				t.Errorf("status = %v, want %v", got.status, tc.want)
			}
			if got.report == "" {
				t.Error("no report was produced; the UI and the alert would have nothing to show")
			}
			if got.status == fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED && len(got.checks) == 0 {
				t.Error("a failure carries no check result, so the UI cannot say what failed")
			}
		})
	}
}

// TestAFailedRestoreNeverReportsVerified is the assertion worth having even though it looks
// tautological: no classification path may ever produce a green verification.
func TestAFailedRestoreNeverReportsVerified(t *testing.T) {
	for code := range fwv1.ErrorCode_name {
		got := classifyRestoreOutcome(&restoreError{plugin: &fwv1.PluginError{
			Code: fwv1.ErrorCode(code), Message: "something went wrong",
		}})
		if got.status == fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
			t.Errorf("a restore that failed with %v was reported as VERIFIED", fwv1.ErrorCode(code))
		}
	}
}

func TestValidateChecks(t *testing.T) {
	caps := &fwv1.Capabilities{
		EngineType:             "testengine",
		SupportsSandboxRestore: true,
		SupportedVerificationChecks: []fwv1.VerificationCheck{
			fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
		},
	}

	tests := []struct {
		name    string
		checks  []fwv1.VerificationCheck
		wantErr error
	}{
		{"empty runs whatever the plugin offers", nil, nil},
		{
			name:   "a declared check is accepted",
			checks: []fwv1.VerificationCheck{fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS},
		},
		{
			// Rejected here rather than mid-run, so a sandbox is not pulled and started only to be
			// told the check does not exist.
			name:    "an undeclared check is refused",
			checks:  []fwv1.VerificationCheck{fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY},
			wantErr: ErrUnsupported,
		},
		{
			name:    "an unspecified check is refused",
			checks:  []fwv1.VerificationCheck{fwv1.VerificationCheck_VERIFICATION_CHECK_UNSPECIFIED},
			wantErr: ErrInvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChecks(caps, tc.checks)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateChecks: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestCheckResultsSurviveTheJSONBRoundTrip protects the discrepancies, which are the only part of a
// verification an operator can act on. A summary that survives while the per-table numbers are lost
// would leave "verification failed" with nothing behind it.
func TestCheckResultsSurviveTheJSONBRoundTrip(t *testing.T) {
	checks := []*fwv1.CheckResult{
		{
			Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			Passed:   true,
			Severity: fwv1.Severity_SEVERITY_INFO,
			Message:  "the restored instance accepts connections and reports version 16.2",
		},
		{
			Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
			Passed:   false,
			Severity: fwv1.Severity_SEVERITY_CRITICAL,
			Message:  "1 object holds the wrong number of rows",
			Discrepancies: []*fwv1.Discrepancy{{
				Database: "app", ObjectName: "public.orders", Expected: 120, Actual: 118,
				Detail: "the restored copy holds a different number of rows",
			}},
		},
	}

	encoded, err := encodeCheckResults(checks)
	if err != nil {
		t.Fatalf("encodeCheckResults: %v", err)
	}

	// A plain array, so `SELECT jsonb_array_length(checks)` works in the psql session an operator
	// will actually run.
	var raw []json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("the stored document is not a JSON array: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("stored %d elements, want 2", len(raw))
	}

	svc := &Service{log: discardLogger()}
	decoded := svc.decodeCheckResults(t.Context(), "v1", encoded)
	if len(decoded) != 2 {
		t.Fatalf("decoded %d checks, want 2", len(decoded))
	}

	d := decoded[1].GetDiscrepancies()
	if len(d) != 1 || d[0].GetExpected() != 120 || d[0].GetActual() != 118 {
		t.Errorf("the discrepancy did not survive: %v", d)
	}
	if decoded[0].GetPassed() != true || decoded[1].GetPassed() != false {
		t.Error("the pass/fail flags did not survive the round trip")
	}
}

func TestEncodeCheckResultsOfNothingIsAnEmptyArray(t *testing.T) {
	// Not SQL NULL: the column is NOT NULL, and an inconclusive verification legitimately ran no
	// checks at all.
	encoded, err := encodeCheckResults(nil)
	if err != nil {
		t.Fatalf("encodeCheckResults: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("encoded = %q, want []", encoded)
	}
}

func TestVerificationStatusRoundTripsThroughTheColumn(t *testing.T) {
	for _, status := range []fwv1.VerificationStatus{
		fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED,
		fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED,
		fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
	} {
		name := verificationStateName(status)
		if got := parseVerificationStatus(name); got != status {
			t.Errorf("%v stored as %q read back as %v", status, name, got)
		}
	}

	// A verification with no conclusion yet has no contract status. Reading 'running' as VERIFIED
	// would show a green tick over a verification that has not finished.
	for _, name := range []string{"pending", "running"} {
		if got := parseVerificationStatus(name); got != fwv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED {
			t.Errorf("%q read back as %v, want UNSPECIFIED", name, got)
		}
	}
}

// TestAnUnspecifiedStatusIsNeverStoredAsVerified guards the other direction: a plugin that answers
// without a status has said nothing, and "nothing" must not be written down as a good backup.
func TestAnUnspecifiedStatusIsNeverStoredAsVerified(t *testing.T) {
	if got := verificationStateName(fwv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED); got != "inconclusive" {
		t.Errorf("an unspecified status is stored as %q, want inconclusive", got)
	}
}

func TestDecodeMetadataTolerated(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"a real document", `{"format":"custom","database":"app"}`, 2},
		{"empty", `{}`, 0},
		{"nothing", "", 0},
		// Core never interprets this map, so a shape it cannot read must not stop a verification.
		{"unreadable", `{"format":{"nested":true}}`, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeMetadata([]byte(tc.raw)); len(got) != tc.want {
				t.Errorf("decodeMetadata(%q) has %d keys, want %d", tc.raw, len(got), tc.want)
			}
		})
	}
}

func TestInconclusiveOutcomeExplainsItself(t *testing.T) {
	got := inconclusiveOutcome("the sandbox never became ready after %s", "3m")
	if got.status != fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
		t.Fatalf("status = %v", got.status)
	}
	if !strings.Contains(got.report, "3m") || got.errMsg == "" {
		t.Errorf("the reason did not reach the report or the error message: %+v", got)
	}
}
