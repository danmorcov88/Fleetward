package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// client talks to the control plane's REST API.
//
// The CLI never opens a connection to the metadata store. Doing so would duplicate authorization in
// a second place and put the metadata database's password on every operator's laptop; going through
// the API means the server stays the only thing holding credentials (ADR-0008).
type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string, timeout time.Duration) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// get performs a GET and decodes the response into out.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// post performs a POST with a JSON body.
func (c *client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// delete performs a DELETE.
func (c *client) delete(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodDelete, path, nil, out)
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contact control plane at %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded so a misconfigured --server pointing at something that streams cannot exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return apiError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// pageQuery builds the query string for the next page of a listing.
func pageQuery(token string) url.Values {
	if token == "" {
		return nil
	}
	return url.Values{"page_token": {token}}
}

// apiError renders a problem-details body as a CLI error, falling back to the raw payload when the
// server returned something else.
func apiError(statusCode int, raw []byte) error {
	var problem struct {
		Title     string `json:"title"`
		Detail    string `json:"detail"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil || problem.Title == "" {
		return fmt.Errorf("control plane returned %d: %s", statusCode, strings.TrimSpace(string(raw)))
	}

	msg := problem.Title
	if problem.Detail != "" {
		msg = problem.Detail
	}
	if problem.RequestID != "" {
		return fmt.Errorf("%s (request %s)", msg, problem.RequestID)
	}
	return fmt.Errorf("%s", msg)
}

// -----------------------------------------------------------------------------------------------
// Wire types
//
// Hand-written rather than reusing the generated protobuf structs: the CLI consumes the REST API
// exactly as any other client would, so the types it needs are the JSON ones, and keeping them
// small documents which fields the command line actually depends on.
// -----------------------------------------------------------------------------------------------

type environment struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	IsProduction bool      `json:"is_production"`
	CreatedAt    time.Time `json:"created_at"`
}

type instance struct {
	ID            string            `json:"id"`
	EnvironmentID string            `json:"environment_id"`
	Name          string            `json:"name"`
	EngineType    string            `json:"engine_type"`
	EngineVersion string            `json:"engine_version"`
	Host          string            `json:"host"`
	Port          int32             `json:"port"`
	Health        string            `json:"health"`
	LastSeenAt    *time.Time        `json:"last_seen_at"`
	Labels        map[string]string `json:"labels"`
	CreatedAt     time.Time         `json:"created_at"`
}

// endpoint renders host:port for display.
func (i instance) endpoint() string { return fmt.Sprintf("%s:%d", i.Host, i.Port) }

type healthSignal struct {
	Name     string  `json:"name"`
	Severity string  `json:"severity"`
	Message  string  `json:"message"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit"`
}

type healthStatus struct {
	State         string         `json:"state"`
	Message       string         `json:"message"`
	Latency       string         `json:"latency"`
	Signals       []healthSignal `json:"signals"`
	EngineVersion string         `json:"engine_version"`
	Error         *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

type serverInfo struct {
	EngineType    string            `json:"engine_type"`
	Version       string            `json:"version"`
	VersionString string            `json:"version_string"`
	Uptime        string            `json:"uptime"`
	ReadOnly      bool              `json:"read_only"`
	Attributes    map[string]string `json:"attributes"`
}

type databaseInfo struct {
	Name        string `json:"name"`
	SizeBytes   string `json:"size_bytes"`
	Owner       string `json:"owner"`
	ObjectCount string `json:"object_count"`
	IsSystem    bool   `json:"is_system"`
}

type node struct {
	Host   string `json:"host"`
	Port   int32  `json:"port"`
	Role   string `json:"role"`
	State  string `json:"state"`
	IsSelf bool   `json:"is_self"`
}

type topology struct {
	Nodes        []node `json:"nodes"`
	IsStandalone bool   `json:"is_standalone"`
}

type objectRef struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type checksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// backupRow mirrors the REST shape of a Backup. Protobuf JSON renders int64 as a string, which is
// why the byte counts are strings here rather than numbers.
type backupRow struct {
	ID               string     `json:"id"`
	InstanceID       string     `json:"instance_id"`
	MethodID         string     `json:"method_id"`
	State            string     `json:"state"`
	SizeBytes        string     `json:"size_bytes"`
	Checksum         *checksum  `json:"checksum"`
	StartedAt        *time.Time `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at"`
	Duration         string     `json:"duration"`
	ConsistencyPoint *time.Time `json:"consistency_point"`
	Artifact         *objectRef `json:"artifact"`
	ErrorMessage     string     `json:"error_message"`
	// Verification is the second half of the two-part status: a backup that succeeded and a
	// verification that failed is the loudest thing this product can report.
	Verification *verification `json:"verification"`
}

type manifestEntry struct {
	Database    string `json:"database"`
	ObjectName  string `json:"object_name"`
	RecordCount string `json:"record_count"`
	SizeBytes   string `json:"size_bytes"`
}

type sourceManifest struct {
	CapturedAt   *time.Time      `json:"captured_at"`
	Entries      []manifestEntry `json:"entries"`
	TotalObjects string          `json:"total_objects"`
	TotalRecords string          `json:"total_records"`
	IsSampled    bool            `json:"is_sampled"`
}

type discrepancy struct {
	Database   string `json:"database"`
	ObjectName string `json:"object_name"`
	Expected   string `json:"expected"`
	Actual     string `json:"actual"`
	Detail     string `json:"detail"`
}

type checkResult struct {
	Check         string        `json:"check"`
	Passed        bool          `json:"passed"`
	Severity      string        `json:"severity"`
	Message       string        `json:"message"`
	Discrepancies []discrepancy `json:"discrepancies"`
	Duration      string        `json:"duration"`
}

type verification struct {
	ID           string        `json:"id"`
	BackupID     string        `json:"backup_id"`
	Status       string        `json:"status"`
	Checks       []checkResult `json:"checks"`
	StartedAt    *time.Time    `json:"started_at"`
	CompletedAt  *time.Time    `json:"completed_at"`
	Duration     string        `json:"duration"`
	Report       string        `json:"report"`
	ErrorMessage string        `json:"error_message"`
}

type verificationResponse struct {
	Verification verification `json:"verification"`
}

type backupResponse struct {
	Backup   backupRow       `json:"backup"`
	Manifest *sourceManifest `json:"manifest"`
}

// trimEnum strips a protobuf enum's type prefix for display: HEALTH_STATE_UP reads as UP.
func trimEnum(prefix, value string) string {
	if value == "" {
		return "UNKNOWN"
	}
	return strings.TrimPrefix(value, prefix)
}

// parseInt64 reads a protobuf JSON integer, which arrives as a string. An unparseable value reads
// as zero: a display helper must not fail a command.
func parseInt64(raw string) int64 {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// orZero renders a protobuf JSON integer, defaulting an empty field to "0".
func orZero(raw string) string {
	if raw == "" {
		return "0"
	}
	return raw
}

// humanBytes renders a byte count at the scale an operator reads it in.
func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
