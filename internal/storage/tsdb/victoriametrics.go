package tsdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/snappy"
	"google.golang.org/protobuf/proto"

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/tsdb/prompb"
)

// maxSamplesPerRequest bounds one remote-write request. VictoriaMetrics accepts far larger
// payloads, but capping keeps a single failed request from losing an entire collection cycle for
// the whole estate.
const maxSamplesPerRequest = 5000

// VictoriaMetrics implements Store against a VictoriaMetrics single-node instance.
type VictoriaMetrics struct {
	client         *http.Client
	remoteWriteURL string
	queryURL       string
	username       string
	password       string
}

var _ Store = (*VictoriaMetrics)(nil)

// NewVictoriaMetrics builds a client from configuration.
func NewVictoriaMetrics(cfg config.TSDBConfig) (*VictoriaMetrics, error) {
	if cfg.RemoteWriteURL == "" {
		return nil, fmt.Errorf("tsdb: remote write URL is required")
	}
	if cfg.QueryURL == "" {
		return nil, fmt.Errorf("tsdb: query URL is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &VictoriaMetrics{
		client:         &http.Client{Timeout: timeout},
		remoteWriteURL: cfg.RemoteWriteURL,
		queryURL:       strings.TrimRight(cfg.QueryURL, "/"),
		username:       cfg.Username,
		password:       cfg.Password,
	}, nil
}

// Write sends samples using the Prometheus remote-write protocol: a protobuf WriteRequest,
// snappy-compressed, POSTed with the documented headers.
func (v *VictoriaMetrics) Write(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}

	for chunk := range chunks(samples, maxSamplesPerRequest) {
		if err := v.writeChunk(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (v *VictoriaMetrics) writeChunk(ctx context.Context, samples []Sample) error {
	req := &prompb.WriteRequest{Timeseries: make([]*prompb.TimeSeries, 0, len(samples))}

	for _, s := range samples {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("tsdb: %w", err)
		}
		// NaN and Inf are legal in the wire format but poison downstream aggregation, and they
		// almost always mean a collector divided by a zero it did not expect. Drop rather than
		// store them.
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}

		pairs := sortedLabelPairs(s)
		labels := make([]*prompb.Label, 0, len(pairs))
		for _, p := range pairs {
			labels = append(labels, &prompb.Label{Name: p.Name, Value: p.Value})
		}

		req.Timeseries = append(req.Timeseries, &prompb.TimeSeries{
			Labels:  labels,
			Samples: []*prompb.Sample{{Value: s.Value, Timestamp: s.At.UnixMilli()}},
		})
	}

	if len(req.Timeseries) == 0 {
		return nil
	}

	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("tsdb: marshal write request: %w", err)
	}
	compressed := snappy.Encode(nil, raw)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, v.remoteWriteURL, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("tsdb: build write request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	v.applyAuth(httpReq)

	resp, err := v.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("tsdb: remote write: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("tsdb: remote write returned %s: %s", resp.Status, readBodySnippet(resp.Body))
	}
	// Draining lets the connection be reused rather than closed after every collection cycle.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Query evaluates a PromQL expression at one instant.
func (v *VictoriaMetrics) Query(ctx context.Context, expr string, at time.Time) ([]QueryResult, error) {
	params := url.Values{
		"query": {expr},
		"time":  {strconv.FormatInt(at.Unix(), 10)},
	}
	return v.doQuery(ctx, "/api/v1/query", params)
}

// QueryRange evaluates a PromQL expression across a range.
func (v *VictoriaMetrics) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]QueryResult, error) {
	if step <= 0 {
		return nil, fmt.Errorf("tsdb: step must be positive")
	}
	if !end.After(start) {
		return nil, fmt.Errorf("tsdb: end must be after start")
	}
	params := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return v.doQuery(ctx, "/api/v1/query_range", params)
}

// promResponse is the Prometheus HTTP API envelope.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// Instant queries return a single [ts, "value"] pair; range queries return many.
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func (v *VictoriaMetrics) doQuery(ctx context.Context, path string, params url.Values) ([]QueryResult, error) {
	endpoint := v.queryURL + path + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("tsdb: build query request: %w", err)
	}
	v.applyAuth(req)

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tsdb: query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tsdb: query returned %s: %s", resp.Status, readBodySnippet(resp.Body))
	}

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("tsdb: decode query response: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("tsdb: query failed (%s): %s", parsed.ErrorType, parsed.Error)
	}

	out := make([]QueryResult, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		result := QueryResult{Labels: r.Metric}
		if len(r.Value) > 0 {
			p, err := parsePoint(r.Value)
			if err != nil {
				return nil, err
			}
			result.Values = append(result.Values, p)
		}
		for _, raw := range r.Values {
			p, err := parsePoint(raw)
			if err != nil {
				return nil, err
			}
			result.Values = append(result.Values, p)
		}
		out = append(out, result)
	}
	return out, nil
}

// parsePoint decodes a Prometheus [timestamp, "value"] pair, where the timestamp is a JSON number
// with fractional seconds and the value is a quoted string.
func parsePoint(raw []json.RawMessage) (Point, error) {
	if len(raw) != 2 {
		return Point{}, fmt.Errorf("tsdb: malformed sample: expected 2 elements, got %d", len(raw))
	}

	var ts float64
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return Point{}, fmt.Errorf("tsdb: parse sample timestamp: %w", err)
	}

	var valueStr string
	if err := json.Unmarshal(raw[1], &valueStr); err != nil {
		return Point{}, fmt.Errorf("tsdb: parse sample value: %w", err)
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return Point{}, fmt.Errorf("tsdb: parse sample value %q: %w", valueStr, err)
	}

	sec, frac := math.Modf(ts)
	return Point{
		At:    time.Unix(int64(sec), int64(frac*1e9)).UTC(),
		Value: value,
	}, nil
}

// HealthCheck probes the store's own health endpoint.
func (v *VictoriaMetrics) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.queryURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("tsdb: build health request: %w", err)
	}
	v.applyAuth(req)

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("tsdb: unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("tsdb: health returned %s", resp.Status)
	}
	return nil
}

// Close implements Store.
func (v *VictoriaMetrics) Close() error {
	v.client.CloseIdleConnections()
	return nil
}

func (v *VictoriaMetrics) applyAuth(req *http.Request) {
	if v.username != "" || v.password != "" {
		req.SetBasicAuth(v.username, v.password)
	}
}

// readBodySnippet reads a bounded prefix of an error response, so a misconfigured URL that returns
// a large HTML page cannot produce a megabyte-long error message.
func readBodySnippet(r io.Reader) string {
	buf := make([]byte, 512)
	n, _ := io.ReadFull(io.LimitReader(r, int64(len(buf))), buf)
	return strings.TrimSpace(string(buf[:n]))
}

// chunks yields successive slices of at most size elements.
func chunks[T any](items []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(items); start += size {
			end := min(start+size, len(items))
			if !yield(items[start:end]) {
				return
			}
		}
	}
}
