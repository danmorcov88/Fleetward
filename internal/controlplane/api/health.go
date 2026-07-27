// Package api hosts the control plane's HTTP and gRPC surface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Checker reports whether one dependency is usable.
type Checker interface {
	HealthCheck(ctx context.Context) error
}

// CheckerFunc adapts a function to Checker.
type CheckerFunc func(ctx context.Context) error

// HealthCheck implements Checker.
func (f CheckerFunc) HealthCheck(ctx context.Context) error { return f(ctx) }

// component is one named dependency in the readiness set.
type component struct {
	name    string
	checker Checker
	// critical marks a dependency the control plane cannot function without. A non-critical
	// failure degrades readiness rather than failing it: a broken MongoDB plugin should not stop
	// an operator from managing their PostgreSQL estate.
	critical bool
}

// Health aggregates dependency checks behind /healthz and /readyz.
//
// The two endpoints answer genuinely different questions, and conflating them causes real damage:
//
//   - /healthz is liveness. It answers "is this process wedged?". It touches nothing external,
//     because a restart cannot fix an unreachable database and restart loops during a brief
//     outage make everything worse.
//   - /readyz is readiness. It answers "should traffic come here?", and it does check dependencies.
type Health struct {
	log        *slog.Logger
	components []component
	timeout    time.Duration
}

// NewHealth builds a health reporter.
func NewHealth(log *slog.Logger, timeout time.Duration) *Health {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Health{log: log, timeout: timeout}
}

// Register adds a dependency to the readiness set.
func (h *Health) Register(name string, critical bool, checker Checker) {
	h.components = append(h.components, component{name: name, checker: checker, critical: critical})
}

// Status is the readiness response body.
type Status struct {
	Status     string            `json:"status"`
	Components []ComponentStatus `json:"components,omitempty"`
	CheckedAt  time.Time         `json:"checked_at"`
}

// ComponentStatus is one dependency's result.
type ComponentStatus struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Critical bool   `json:"critical"`
	Error    string `json:"error,omitempty"`
	// LatencyMS is how long the check took, which is often the first sign of trouble before
	// anything actually fails.
	LatencyMS int64 `json:"latency_ms"`
}

const (
	statusHealthy   = "healthy"
	statusDegraded  = "degraded"
	statusUnhealthy = "unhealthy"
)

// LivenessHandler serves /healthz.
func (h *Health) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{Status: statusHealthy, CheckedAt: time.Now().UTC()})
	}
}

// ReadinessHandler serves /readyz.
func (h *Health) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := h.Check(r.Context())

		code := http.StatusOK
		if status.Status == statusUnhealthy {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, status)
	}
}

// Check runs every registered check concurrently and aggregates the result.
func (h *Health) Check(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	results := make([]ComponentStatus, len(h.components))

	var wg sync.WaitGroup
	for i, c := range h.components {
		wg.Add(1)
		go func() {
			defer wg.Done()

			start := time.Now()
			err := c.checker.HealthCheck(ctx)
			result := ComponentStatus{
				Name:      c.name,
				Status:    statusHealthy,
				Critical:  c.critical,
				LatencyMS: time.Since(start).Milliseconds(),
			}
			if err != nil {
				result.Status = statusUnhealthy
				result.Error = err.Error()
			}
			results[i] = result
		}()
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	overall := statusHealthy
	for _, r := range results {
		if r.Status != statusUnhealthy {
			continue
		}
		if r.Critical {
			overall = statusUnhealthy
			break
		}
		overall = statusDegraded
	}

	return Status{Status: overall, Components: results, CheckedAt: time.Now().UTC()}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The response is already committed by WriteHeader, so a failed encode can only be logged by
	// the caller's middleware, not turned into an error status.
	_ = json.NewEncoder(w).Encode(body)
}
