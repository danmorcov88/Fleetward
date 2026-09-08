package acts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client talks to the control plane's REST API, and it is the only way the demo changes anything.
//
// Hand-written rather than reusing anything from the tree, for the same reason the CLI's client is:
// the demo consumes the public API exactly as a third party would, so the types it needs are the
// JSON ones. If a field named here stops existing, the demo stops working — which is precisely the
// drift this slice exists to make visible.
type client struct {
	baseURL string
	http    *http.Client
	token   string
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Generous: a verification pulls an image and starts a database, and this client follows it.
		http:  &http.Client{Timeout: 2 * time.Minute},
		token: token,
	}
}

// as returns the same client presenting a different credential. Act 5 is two callers asking for the
// same thing and being answered differently, so the credential is the only thing that varies.
func (c *client) as(token string) *client {
	return &client{baseURL: c.baseURL, http: c.http, token: token}
}

func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	_, err := c.do(ctx, http.MethodGet, path, nil, out)
	return err
}

func (c *client) post(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, http.MethodPost, path, body, out)
	return err
}

// status performs a request and returns the HTTP status instead of an error for it.
//
// Act 5 needs the number rather than a message: "a real 403 that came from the server" is the claim,
// and asserting on prose would let a client-side refusal pass for one.
func (c *client) status(ctx context.Context, method, path string, body any) (int, string, error) {
	code, err := c.do(ctx, method, path, body, nil)
	if err != nil && code == 0 {
		return 0, "", err
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return code, detail, nil
}

func (c *client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, problem(method, path, resp.StatusCode, raw)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

// problem renders the single problem-details shape the API uses for every error.
func problem(method, path string, code int, raw []byte) error {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Title == "" {
		return fmt.Errorf("%s %s returned %d: %s", method, path, code, strings.TrimSpace(string(raw)))
	}
	if p.Detail != "" {
		return fmt.Errorf("%s %s returned %d: %s", method, path, code, p.Detail)
	}
	return fmt.Errorf("%s %s returned %d: %s", method, path, code, p.Title)
}

// -----------------------------------------------------------------------------------------------
// Wire types — only the fields the demo actually reads.
// -----------------------------------------------------------------------------------------------

type environmentRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type instanceRow struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	EngineType    string `json:"engine_type"`
	Health        string `json:"health"`
	HealthMessage string `json:"health_message"`
}

type checksumRow struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type objectRefRow struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type verificationRow struct {
	ID          string     `json:"id"`
	BackupID    string     `json:"backup_id"`
	Status      string     `json:"status"`
	Report      string     `json:"report"`
	CompletedAt *time.Time `json:"completed_at"`
	Checks      []struct {
		Check    string `json:"check"`
		Passed   bool   `json:"passed"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	} `json:"checks"`
}

type backupRow struct {
	ID           string           `json:"id"`
	InstanceID   string           `json:"instance_id"`
	State        string           `json:"state"`
	SizeBytes    string           `json:"size_bytes"`
	Origin       string           `json:"origin"`
	Checksum     *checksumRow     `json:"checksum"`
	Artifact     *objectRefRow    `json:"artifact"`
	CompletedAt  *time.Time       `json:"completed_at"`
	ErrorMessage string           `json:"error_message"`
	Verification *verificationRow `json:"verification"`
}

type adherenceRow struct {
	InstanceID   string     `json:"instance_id"`
	InstanceName string     `json:"instance_name"`
	EngineType   string     `json:"engine_type"`
	State        string     `json:"state"`
	ExpectedCron string     `json:"expected_cron"`
	Deadline     *time.Time `json:"deadline"`
	SatisfiedBy  *backupRow `json:"satisfied_by"`
	LatestBackup *backupRow `json:"latest_backup"`
	Caveats      []string   `json:"caveats"`
}

type retentionCandidateRow struct {
	BackupID        string     `json:"backup_id"`
	InstanceName    string     `json:"instance_name"`
	CompletedAt     *time.Time `json:"completed_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	SizeBytes       string     `json:"size_bytes"`
	ProtectedReason string     `json:"protected_reason"`
}

type auditRow struct {
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Succeeded    bool      `json:"succeeded"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// trimEnum strips a protobuf enum's type prefix for display: HEALTH_STATE_UP reads as UP.
func trimEnum(prefix, value string) string {
	if value == "" {
		return "UNKNOWN"
	}
	return strings.TrimPrefix(value, prefix)
}
