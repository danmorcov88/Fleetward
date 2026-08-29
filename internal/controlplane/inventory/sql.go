package inventory

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

// -----------------------------------------------------------------------------------------------
// Identifiers
// -----------------------------------------------------------------------------------------------

// isUUID reports whether s has the canonical 8-4-4-4-12 hexadecimal form. The check itself is
// shared with every other service that reads a row by identifier; only the error wrapping below is
// this package's.
func isUUID(s string) bool { return metadb.IsUUID(s) }

// requireUUID validates a mandatory identifier.
func requireUUID(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidArgument, field)
	}
	if !isUUID(value) {
		return "", fmt.Errorf("%w: %s must be a UUID", ErrInvalidArgument, field)
	}
	return value, nil
}

// nullableUUID validates an optional filter, returning nil for "no filter".
func nullableUUID(field, value string) (*string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil //nolint:nilnil // nil means "this filter was not supplied"
	}
	if !isUUID(value) {
		return nil, fmt.Errorf("%w: %s must be a UUID", ErrInvalidArgument, field)
	}
	return &value, nil
}

// nullableText turns an empty filter into a SQL NULL.
func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// -----------------------------------------------------------------------------------------------
// Pagination
// -----------------------------------------------------------------------------------------------

// cursor is a keyset position in a listing ordered by (created_at, id).
//
// Keyset rather than offset: an estate is added to while it is being read, and an offset would
// silently skip or repeat rows when that happens.
type cursor struct {
	createdAt time.Time
	id        string
	set       bool
}

func (c cursor) after() *time.Time {
	if !c.set {
		return nil
	}
	return &c.createdAt
}

func (c cursor) afterID() *string {
	if !c.set {
		return nil
	}
	return &c.id
}

// encodeCursor renders the position of the last row on a page.
func encodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a page token. An empty token starts at the beginning.
func decodeCursor(token string) (cursor, error) {
	if token == "" {
		return cursor{}, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: page_token is not a valid cursor", ErrInvalidArgument)
	}
	createdAt, id, ok := strings.Cut(string(raw), "|")
	if !ok || !isUUID(id) {
		return cursor{}, fmt.Errorf("%w: page_token is not a valid cursor", ErrInvalidArgument)
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: page_token is not a valid cursor", ErrInvalidArgument)
	}
	return cursor{createdAt: parsed, id: id, set: true}, nil
}

// clampPageSize applies the default and the ceiling.
func clampPageSize(requested int32) (int32, error) {
	switch {
	case requested < 0:
		return 0, fmt.Errorf("%w: page_size must not be negative", ErrInvalidArgument)
	case requested == 0:
		return defaultPageSize, nil
	case requested > maxPageSize:
		return maxPageSize, nil
	default:
		return requested, nil
	}
}

// -----------------------------------------------------------------------------------------------
// Rows
// -----------------------------------------------------------------------------------------------

// scanInstance reads one instance row in the column order used by every instance listing.
func scanInstance(rows pgx.Rows, tenantID string) (*fwv1.Instance, error) {
	var (
		inst      = &fwv1.Instance{TenantId: tenantID}
		labels    map[string]string
		health    string
		lastSeen  *time.Time
		createdAt time.Time
	)
	if err := rows.Scan(&inst.Id, &inst.EnvironmentId, &inst.Name, &inst.EngineType,
		&inst.EngineVersion, &inst.Host, &inst.Port, &labels, &health, &lastSeen,
		&createdAt); err != nil {
		return nil, fmt.Errorf("inventory: scan instance: %w", err)
	}

	inst.Labels = labels
	inst.Health = parseHealthState(health)
	inst.CreatedAt = timestamppb.New(createdAt)
	if lastSeen != nil {
		inst.LastSeenAt = timestamppb.New(*lastSeen)
	}
	return inst, nil
}

// parseHealthState reads the enum name stored in the instances table. An unrecognized value —
// written by a newer version, or edited by hand — becomes UNKNOWN rather than an error, because a
// row nobody can read is worse than one whose health is uncertain.
func parseHealthState(name string) fwv1.HealthState {
	if v, ok := fwv1.HealthState_value[name]; ok {
		return fwv1.HealthState(v)
	}
	return fwv1.HealthState_HEALTH_STATE_UNKNOWN
}

// labelsOrEmpty keeps a nil map from being written as SQL NULL into a NOT NULL JSONB column.
func labelsOrEmpty(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

// -----------------------------------------------------------------------------------------------
// Error classification
// -----------------------------------------------------------------------------------------------

// isUniqueViolation reports a duplicate key, including the partial unique index that allows at most
// one default connection per instance.
func isUniqueViolation(err error) bool { return metadb.IsUniqueViolation(err) }

// isForeignKeyViolation reports a reference to a row that does not exist, such as an unknown
// environment.
func isForeignKeyViolation(err error) bool { return metadb.IsForeignKeyViolation(err) }
