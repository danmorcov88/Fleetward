// Package tsdb is Fleetward's client for the metrics store (ADR-0006).
//
// Metrics collected from monitored databases are written with the Prometheus remote-write protocol
// and read back with the Prometheus-compatible query API. Everything sits behind the Writer and
// Querier interfaces so that swapping VictoriaMetrics for Mimir, Thanos, or plain Prometheus stays
// a configuration concern.
package tsdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Label names Fleetward attaches to every sample it writes. Keeping them in one place stops the
// collection loop and the query layer from drifting apart on a typo.
const (
	LabelMetricName  = "__name__"
	LabelTenantID    = "fw_tenant_id"
	LabelInstanceID  = "fw_instance_id"
	LabelEnvironment = "fw_environment"
	LabelEngineType  = "fw_engine_type"
)

// Sample is one metric observation, ready to be written.
type Sample struct {
	// Name is an OpenTelemetry semantic-convention metric name (ADR-0011), for example
	// "db.client.connection.count". It is translated to a Prometheus-legal name on write.
	Name   string
	Labels map[string]string
	Value  float64
	At     time.Time
}

// Validate reports whether the sample can be written.
func (s Sample) Validate() error {
	if s.Name == "" {
		return errors.New("sample: name is required")
	}
	if s.At.IsZero() {
		return errors.New("sample: timestamp is required")
	}
	return nil
}

// Writer ingests metric samples.
type Writer interface {
	// Write sends samples to the metrics store. Implementations should batch internally rather
	// than requiring callers to size their own batches.
	Write(ctx context.Context, samples []Sample) error
	HealthCheck(ctx context.Context) error
	Close() error
}

// QueryResult is one time series returned by a query.
type QueryResult struct {
	Labels map[string]string
	// Values holds a single point for an instant query, or the full series for a range query.
	Values []Point
}

// Point is one observation in a query result.
type Point struct {
	At    time.Time
	Value float64
}

// Querier reads metrics back out.
type Querier interface {
	// Query evaluates a PromQL expression at a single instant.
	Query(ctx context.Context, expr string, at time.Time) ([]QueryResult, error)
	// QueryRange evaluates a PromQL expression over a time range at the given step.
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]QueryResult, error)
	HealthCheck(ctx context.Context) error
}

// Store combines both halves for callers that need each.
type Store interface {
	Writer
	Querier
}

// PromName converts an OpenTelemetry metric name to a Prometheus-legal one.
//
// OTel names are dotted ("db.client.connection.count"); Prometheus permits only
// [a-zA-Z_:][a-zA-Z0-9_:]*. This is the standard OTel-to-Prometheus mapping, applied in one place
// so that the name written and the name queried can never disagree.
func PromName(otelName string) string {
	if otelName == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(otelName))
	for i := range len(otelName) {
		c := otelName[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
			b.WriteByte(c)
		case c >= '0' && c <= '9':
			// A leading digit would make the name illegal.
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// PromLabelName converts an attribute key to a Prometheus-legal label name.
func PromLabelName(key string) string {
	name := PromName(key)
	// Prometheus reserves the "__" prefix for internal labels.
	if strings.HasPrefix(name, "__") {
		return "attr" + name
	}
	return name
}

// sortedLabelPairs returns a sample's labels in the sorted order remote-write requires, with the
// metric name included as __name__.
func sortedLabelPairs(s Sample) []labelPair {
	pairs := make([]labelPair, 0, len(s.Labels)+1)
	pairs = append(pairs, labelPair{Name: LabelMetricName, Value: PromName(s.Name)})
	for k, v := range s.Labels {
		if k == "" || v == "" {
			// Prometheus treats an empty label value as the label being absent; writing it wastes
			// space and can produce two series that look identical in the UI.
			continue
		}
		pairs = append(pairs, labelPair{Name: PromLabelName(k), Value: v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Name < pairs[j].Name })
	return pairs
}

type labelPair struct {
	Name  string
	Value string
}

// ErrEmptyBatch is returned when a write is asked to send nothing.
var ErrEmptyBatch = fmt.Errorf("tsdb: empty batch")
