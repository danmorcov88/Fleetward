// Package identity owns users, the credentials that authenticate them, the grants that say what
// they may do, and the record of what everybody did.
//
// It is deliberately the smallest surface that makes B6 operable rather than merely enforced: an
// installation where authorization works and nobody can issue a second credential is an
// installation with one administrator and no colleagues.
//
// Three shapes are worth knowing before reading further.
//
// **A token belongs to a user, and issuing one to somebody who does not exist yet creates them.**
// `users` is provisioned from OIDC claims on first login (ADR-0008), and B10 will do exactly that.
// Until then the first sight Fleetward has of a person is somebody issuing them a credential, so
// that is where the row appears. The email is the identity, matching `users.subject`'s uniqueness
// per tenant.
//
// **The secret is returned once.** Only its SHA-256 is stored, so there is no path — not for an
// administrator, not for a database superuser — that reads a token back out.
//
// **Nothing here deletes anything.** Revoking a token stamps `revoked_at`; the row stays so the
// audit log's references resolve and so the revocation is itself evidence.
package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/controlplane/authz"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

// Sentinel errors, classified with errors.Is by the gRPC layer, which is the only place that
// decides what a client sees.
var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidArgument = errors.New("invalid argument")
)

const (
	defaultAuditPageSize = 50
	maxAuditPageSize     = 500
)

// Service is the identity domain service.
type Service struct {
	pool   *pgxpool.Pool
	tokens *authn.TokenStore
	log    *slog.Logger
}

// New builds the service.
func New(pool *pgxpool.Pool, tokens *authn.TokenStore, log *slog.Logger) *Service {
	return &Service{pool: pool, tokens: tokens, log: log.With(slog.String("component", "identity"))}
}

// -----------------------------------------------------------------------------------------------
// Who am I
// -----------------------------------------------------------------------------------------------

// Me reports the caller and the grants it holds.
func (s *Service) Me(ctx context.Context) (*fwv1.GetMeResponse, error) {
	p, err := authn.MustFrom(ctx)
	if err != nil {
		return nil, err
	}

	resp := &fwv1.GetMeResponse{
		Caller: &fwv1.Caller{
			Kind:        callerKind(p.Kind),
			UserId:      p.UserID,
			Actor:       p.Actor,
			DisplayName: p.DisplayName,
			Email:       p.Email,
			TenantId:    p.TenantID,
		},
		Grants:      grantsToProto(p.Grants),
		HighestRole: authz.HighestRole(p.Grants),
	}

	// A bootstrap caller holds no rows, and reporting "no role" for a credential that is
	// tenant-wide admin would be a lie the UI would then render.
	if p.Kind == authn.KindBootstrap || p.Kind == authn.KindSystem {
		resp.HighestRole = authz.RoleAdmin
		resp.Grants = []*fwv1.RoleGrant{{Role: authz.RoleAdmin}}
	}
	return resp, nil
}

func callerKind(k authn.Kind) fwv1.CallerKind {
	switch k {
	case authn.KindUser:
		return fwv1.CallerKind_CALLER_KIND_USER
	case authn.KindSystem:
		return fwv1.CallerKind_CALLER_KIND_SYSTEM
	case authn.KindBootstrap:
		return fwv1.CallerKind_CALLER_KIND_BOOTSTRAP
	default:
		return fwv1.CallerKind_CALLER_KIND_UNSPECIFIED
	}
}

func grantsToProto(grants []authn.Grant) []*fwv1.RoleGrant {
	out := make([]*fwv1.RoleGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, &fwv1.RoleGrant{
			Role:          g.Role,
			Rank:          int32(g.Rank), //nolint:gosec // G115: a seeded rank is 10, 20, 30 or 40
			EnvironmentId: g.EnvironmentID,
			InstanceId:    g.InstanceID,
		})
	}
	return out
}

// -----------------------------------------------------------------------------------------------
// Tokens
// -----------------------------------------------------------------------------------------------

// CreateTokenInput describes a credential to issue.
type CreateTokenInput struct {
	Email         string
	DisplayName   string
	Role          string
	EnvironmentID string
	InstanceID    string
	Description   string
	TTL           time.Duration
}

// CreateToken mints a credential, creating the user and the grant if they are new.
//
// All of it in one transaction: a token whose user exists but whose grant does not is a credential
// that authenticates and can do nothing, which is the most confusing possible outcome of a command
// that appeared to succeed.
func (s *Service) CreateToken(ctx context.Context, in CreateTokenInput) (*fwv1.ApiToken, string, error) {
	tenantID := authn.Tenant(ctx)
	email := strings.TrimSpace(strings.ToLower(in.Email))
	role := strings.TrimSpace(strings.ToLower(in.Role))

	switch {
	case email == "":
		return nil, "", fmt.Errorf("%w: email is required", ErrInvalidArgument)
	case role == "":
		return nil, "", fmt.Errorf("%w: role is required", ErrInvalidArgument)
	case in.EnvironmentID != "" && in.InstanceID != "":
		// The schema says this too, as role_grants_single_scope. Saying it here as well means the
		// operator gets a sentence rather than a constraint violation.
		return nil, "", fmt.Errorf(
			"%w: a grant is scoped to an environment or to an instance, never both", ErrInvalidArgument)
	}

	// The role must be one the database knows: role_grants.role_name is ON DELETE RESTRICT against
	// `roles`, so an unknown one would fail on insert anyway, with a worse message.
	var rank int
	err := s.pool.QueryRow(ctx, `SELECT rank FROM roles WHERE name = $1`, role).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: unknown role %q; the seeded roles are viewer, operator, dba, admin",
			ErrInvalidArgument, role)
	}
	if err != nil {
		return nil, "", fmt.Errorf("look up role: %w", err)
	}

	if in.EnvironmentID != "" && !metadb.IsUUID(in.EnvironmentID) {
		return nil, "", fmt.Errorf("%w: environment_id must be a UUID", ErrInvalidArgument)
	}
	if in.InstanceID != "" && !metadb.IsUUID(in.InstanceID) {
		return nil, "", fmt.Errorf("%w: instance_id must be a UUID", ErrInvalidArgument)
	}

	presented, tokenID, hash, err := authn.NewToken()
	if err != nil {
		return nil, "", err
	}

	caller, _ := authn.From(ctx)
	var createdBy *string
	if caller.UserID != "" {
		createdBy = &caller.UserID
	}
	var expiresAt *time.Time
	if in.TTL > 0 {
		t := time.Now().Add(in.TTL)
		expiresAt = &t
	}

	var out *fwv1.ApiToken
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		displayName := strings.TrimSpace(in.DisplayName)
		if displayName == "" {
			displayName = email
		}

		// The subject is the email until an identity provider supplies a real one. B10 replaces
		// this with the `sub` claim, and the UNIQUE (tenant_id, subject) that has been in the
		// schema since migration 000001 is what makes that a matter of updating a column.
		var userID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (tenant_id, subject, email, display_name)
			VALUES ($1, $2, $2, $3)
			ON CONFLICT (tenant_id, subject)
			DO UPDATE SET display_name = EXCLUDED.display_name, is_active = TRUE, updated_at = now()
			RETURNING id::text`, tenantID, email, displayName).Scan(&userID); err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}

		// NOT EXISTS rather than ON CONFLICT: `role_grants` has no unique constraint, deliberately —
		// the same user may hold several grants at different scopes, which is the whole point of
		// scoping. What must not happen is the *same* grant accumulating a row per token issued,
		// which is what an unguarded INSERT would do to anybody given a second credential.
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_grants (tenant_id, user_id, role_name, environment_id, instance_id, granted_by)
			SELECT $1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, $6
			WHERE NOT EXISTS (
			    SELECT 1 FROM role_grants
			    WHERE tenant_id = $1 AND user_id = $2 AND role_name = $3
			      AND environment_id IS NOT DISTINCT FROM NULLIF($4, '')::uuid
			      AND instance_id IS NOT DISTINCT FROM NULLIF($5, '')::uuid)`,
			tenantID, userID, role, in.EnvironmentID, in.InstanceID, createdBy); err != nil {
			return fmt.Errorf("grant role: %w", err)
		}

		var id string
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO api_tokens (tenant_id, user_id, token_id, token_hash, description, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id::text, created_at`,
			tenantID, userID, tokenID, hash, strings.TrimSpace(in.Description), createdBy, expiresAt).
			Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("store token: %w", err)
		}

		out = &fwv1.ApiToken{
			Id:          id,
			UserId:      userID,
			Description: strings.TrimSpace(in.Description),
			CreatedAt:   timestamppb.New(createdAt),
			DisplayName: displayName,
			Email:       email,
		}
		if expiresAt != nil {
			out.ExpiresAt = timestamppb.New(*expiresAt)
		}
		if createdBy != nil {
			out.CreatedBy = *createdBy
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	grants, err := authn.LoadGrants(ctx, s.pool, tenantID, out.GetUserId())
	if err != nil {
		return nil, "", err
	}
	out.Grants = grantsToProto(grants)

	return out, presented, nil
}

// ListTokens reports the credentials this tenant has issued, with what each may do.
func (s *Service) ListTokens(ctx context.Context, includeInactive bool) ([]*fwv1.ApiToken, error) {
	filter := ""
	if !includeInactive {
		filter = ` AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now())`
	}

	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.user_id::text, t.description, COALESCE(t.created_by::text, ''),
		       t.created_at, t.expires_at, t.last_used_at, t.revoked_at, u.display_name, u.email
		FROM   api_tokens t
		JOIN   users u ON u.id = t.user_id
		WHERE  t.tenant_id = $1`+filter+`
		ORDER  BY t.created_at DESC`, authn.Tenant(ctx))
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.ApiToken
	for rows.Next() {
		var (
			t                                fwv1.ApiToken
			createdAt                        time.Time
			expiresAt, lastUsedAt, revokedAt *time.Time
			description, createdBy           string
			displayName, email, id, userID   string
		)
		if err := rows.Scan(&id, &userID, &description, &createdBy, &createdAt,
			&expiresAt, &lastUsedAt, &revokedAt, &displayName, &email); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		t.Id, t.UserId, t.Description, t.CreatedBy = id, userID, description, createdBy
		t.DisplayName, t.Email = displayName, email
		t.CreatedAt = timestamppb.New(createdAt)
		if expiresAt != nil {
			t.ExpiresAt = timestamppb.New(*expiresAt)
		}
		if lastUsedAt != nil {
			t.LastUsedAt = timestamppb.New(*lastUsedAt)
		}
		if revokedAt != nil {
			t.RevokedAt = timestamppb.New(*revokedAt)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}

	for _, t := range out {
		grants, err := authn.LoadGrants(ctx, s.pool, authn.Tenant(ctx), t.GetUserId())
		if err != nil {
			return nil, err
		}
		t.Grants = grantsToProto(grants)
	}
	return out, nil
}

// RevokeToken stops a credential working.
//
// Revoking one already revoked is not an error. An operator who is not sure whether a token was
// dealt with should be able to make sure without reading a failure, and the second call changes
// nothing.
func (s *Service) RevokeToken(ctx context.Context, tokenID string) error {
	if !metadb.IsUUID(tokenID) {
		return fmt.Errorf("%w: token_id must be a UUID", ErrInvalidArgument)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, now())
		WHERE  id = $1 AND tenant_id = $2`, tokenID, authn.Tenant(ctx))
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no token %s", ErrNotFound, tokenID)
	}

	// The verified-principal cache would otherwise keep this credential working for the length of
	// its TTL on this replica. Another replica's cache still will, which is why the TTL is short
	// and why docs/ops/authorization.md says so rather than leaving it to be discovered.
	s.tokens.Forget(tokenID)
	return nil
}

// -----------------------------------------------------------------------------------------------
// The audit log
// -----------------------------------------------------------------------------------------------

// ListAuditInput filters the record.
type ListAuditInput struct {
	Actor        string
	Action       string
	ResourceType string
	ResourceID   string
	FailuresOnly bool
	PageSize     int32
	PageToken    string
}

// ListAuditLog reads the append-only record, newest first.
//
// Paging is by row id rather than by offset. The ids are monotonic and nothing is ever deleted —
// the table refuses DELETE by trigger — so a cursor cannot skip a row or repeat one, which is a
// property an offset does not have on a table being written to while it is read.
func (s *Service) ListAuditLog(ctx context.Context, in ListAuditInput) ([]*fwv1.AuditEntry, string, error) {
	size := int(in.PageSize)
	switch {
	case size <= 0:
		size = defaultAuditPageSize
	case size > maxAuditPageSize:
		size = maxAuditPageSize
	}

	args := []any{authn.Tenant(ctx)}
	filters := ""
	add := func(clause string, value any) {
		args = append(args, value)
		filters += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}
	if in.Actor != "" {
		add("actor =", in.Actor)
	}
	if in.Action != "" {
		add("action =", in.Action)
	}
	if in.ResourceType != "" {
		add("resource_type =", in.ResourceType)
	}
	if in.ResourceID != "" {
		add("resource_id =", in.ResourceID)
	}
	if in.FailuresOnly {
		filters += " AND succeeded = FALSE"
	}
	if in.PageToken != "" {
		var cursor int64
		if _, err := fmt.Sscanf(in.PageToken, "%d", &cursor); err != nil {
			return nil, "", fmt.Errorf("%w: page_token is not valid", ErrInvalidArgument)
		}
		add("id <", cursor)
	}
	args = append(args, size+1)

	rows, err := s.pool.Query(ctx, `
		SELECT id, actor, COALESCE(user_id::text, ''), action, resource_type, resource_id,
		       details, COALESCE(host(source_ip), ''), request_id, succeeded, occurred_at
		FROM   audit_log
		WHERE  tenant_id = $1`+filters+`
		ORDER  BY id DESC
		LIMIT  $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("list audit log: %w", err)
	}
	defer rows.Close()

	var entries []*fwv1.AuditEntry
	for rows.Next() {
		var (
			e          fwv1.AuditEntry
			details    map[string]string
			occurredAt time.Time
		)
		if err := rows.Scan(&e.Id, &e.Actor, &e.UserId, &e.Action, &e.ResourceType, &e.ResourceId,
			&details, &e.SourceIp, &e.RequestId, &e.Succeeded, &occurredAt); err != nil {
			return nil, "", fmt.Errorf("scan audit entry: %w", err)
		}
		e.Details = details
		e.OccurredAt = timestamppb.New(occurredAt)
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read audit log: %w", err)
	}

	next := ""
	if len(entries) > size {
		entries = entries[:size]
		next = fmt.Sprint(entries[len(entries)-1].GetId())
	}
	return entries, next, nil
}
