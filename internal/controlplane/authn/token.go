package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Token format.
//
// A presented credential looks like `fwt_<token_id>_<secret>`. The two halves do different jobs:
// the id is stored in the clear and indexed, so verifying a token is one equality lookup rather
// than a scan comparing every hash in the table; the secret is stored only as its SHA-256.
//
// SHA-256 rather than bcrypt or argon2 is a deliberate choice, argued in ADR-0033: the secret is
// 128 bits from crypto/rand, so there is no dictionary for a work factor to slow down, and a KDF
// would put tens of milliseconds on the hot path of a screen that refetches every thirty seconds
// across fifty rows.
const (
	// TokenPrefix marks a Fleetward credential, so one found in a log or a paste is recognisable
	// for what it is and can be revoked rather than puzzled over.
	TokenPrefix = "fwt"
	// tokenIDBytes and tokenSecretBytes are the two halves, hex-encoded in the presented form.
	tokenIDBytes     = 8
	tokenSecretBytes = 16
)

// ErrMalformedToken reports a credential that is not shaped like one of ours. It is folded into
// ErrInvalidCredential before any client sees it.
var ErrMalformedToken = errors.New("not a fleetward token")

// NewToken mints a credential. It returns the presented form, which is shown to a human exactly
// once, along with the two things that get stored: the public id and the hash of the secret.
func NewToken() (presented, tokenID, hash string, err error) {
	idRaw := make([]byte, tokenIDBytes)
	if _, err := rand.Read(idRaw); err != nil {
		return "", "", "", fmt.Errorf("generate token id: %w", err)
	}
	secretRaw := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return "", "", "", fmt.Errorf("generate token secret: %w", err)
	}

	tokenID = hex.EncodeToString(idRaw)
	secret := hex.EncodeToString(secretRaw)
	return TokenPrefix + "_" + tokenID + "_" + secret, tokenID, hashSecret(secret), nil
}

// splitToken pulls the two halves out of a presented credential.
func splitToken(presented string) (tokenID, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(presented), "_")
	if len(parts) != 3 || parts[0] != TokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", ErrMalformedToken
	}
	return parts[1], parts[2], nil
}

// hashSecret is what is stored and what is compared against.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// fingerprint reduces a whole presented credential to a cache key, so the cache never holds a
// usable secret even in memory.
func fingerprint(presented string) string {
	sum := sha256.Sum256([]byte(presented))
	return hex.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------------------------
// Verification against the database
// -----------------------------------------------------------------------------------------------

// TokenStore resolves a presented credential into a principal, and caches the answer briefly.
type TokenStore struct {
	pool     *pgxpool.Pool
	cacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry

	// lastUsed batches the bookkeeping write so that authenticating a request stays a read.
	usedMu sync.Mutex
	used   map[string]time.Time
}

type cacheEntry struct {
	principal Principal
	expiresAt time.Time
}

// NewTokenStore builds the store.
//
// cacheTTL bounds how long a revoked credential keeps working, so it is deliberately short rather
// than convenient. Every authenticated request would otherwise be a join across `api_tokens`,
// `users` and `role_grants`, on a dashboard that refetches every thirty seconds.
func NewTokenStore(pool *pgxpool.Pool, cacheTTL time.Duration) *TokenStore {
	return &TokenStore{
		pool:     pool,
		cacheTTL: cacheTTL,
		cache:    make(map[string]cacheEntry),
		used:     make(map[string]time.Time),
	}
}

// Verify resolves a presented credential, or reports why it will not.
func (s *TokenStore) Verify(ctx context.Context, presented string) (Principal, error) {
	key := fingerprint(presented)

	if p, ok := s.cached(key); ok {
		s.noteUsed(p.TokenID)
		return p, nil
	}

	tokenID, secret, err := splitToken(presented)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}

	var (
		storedHash  string
		id          string
		userID      string
		tenantID    string
		email       string
		displayName string
		isActive    bool
		expiresAt   *time.Time
		revokedAt   *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.token_hash, t.user_id::text, t.tenant_id::text,
		       u.email, u.display_name, u.is_active, t.expires_at, t.revoked_at
		FROM   api_tokens t
		JOIN   users u ON u.id = t.user_id
		WHERE  t.token_id = $1`, tokenID).
		Scan(&id, &storedHash, &userID, &tenantID, &email, &displayName, &isActive, &expiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, fmt.Errorf("look up token: %w", err)
	}

	// Constant time, even though the id half already narrowed this to one row: a timing signal on
	// the secret is the one thing a comparison here could leak.
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashSecret(secret))) != 1 {
		return Principal{}, ErrInvalidCredential
	}

	// Every reason to refuse produces the same error. Which of them applied is in the server's log,
	// not in the response: telling a caller that a token was "expired" rather than "unknown"
	// confirms that it was once real.
	switch {
	case revokedAt != nil:
		return Principal{}, ErrInvalidCredential
	case expiresAt != nil && expiresAt.Before(time.Now()):
		return Principal{}, ErrInvalidCredential
	case !isActive:
		return Principal{}, ErrInvalidCredential
	}

	grants, err := LoadGrants(ctx, s.pool, tenantID, userID)
	if err != nil {
		return Principal{}, err
	}

	p := Principal{
		Kind:        KindUser,
		UserID:      userID,
		Actor:       email,
		DisplayName: displayName,
		Email:       email,
		TenantID:    tenantID,
		Grants:      grants,
		TokenID:     id,
	}

	s.store(key, p)
	s.noteUsed(id)
	return p, nil
}

func (s *TokenStore) cached(key string) (Principal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return Principal{}, false
	}
	return entry.principal, true
}

func (s *TokenStore) store(key string, p Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bounded by eviction rather than by a policy: the map only ever holds live credentials, and an
	// installation with enough tokens for this to matter has other problems.
	if len(s.cache) > 1024 {
		for k, e := range s.cache {
			if time.Now().After(e.expiresAt) {
				delete(s.cache, k)
			}
		}
	}
	s.cache[key] = cacheEntry{principal: p, expiresAt: time.Now().Add(s.cacheTTL)}
}

// Forget drops a credential from the cache, so a revocation takes effect on the replica that
// performed it immediately rather than after the cache TTL.
func (s *TokenStore) Forget(tokenID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.cache {
		if e.principal.TokenID == tokenID {
			delete(s.cache, k)
		}
	}
}

func (s *TokenStore) noteUsed(tokenID string) {
	if tokenID == "" {
		return
	}
	s.usedMu.Lock()
	defer s.usedMu.Unlock()
	s.used[tokenID] = time.Now()
}

// FlushLastUsed writes the batched last-used timestamps.
//
// Separated from Verify on purpose: `last_used_at` is an operator's convenience — telling a live
// credential from one nobody has touched since it was issued — and paying for a write on every
// authenticated request to maintain it would be the wrong trade on a dashboard that polls.
func (s *TokenStore) FlushLastUsed(ctx context.Context) error {
	s.usedMu.Lock()
	pending := s.used
	s.used = make(map[string]time.Time)
	s.usedMu.Unlock()

	for id, at := range pending {
		if _, err := s.pool.Exec(ctx,
			`UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, at); err != nil {
			return fmt.Errorf("record token use: %w", err)
		}
	}
	return nil
}

// LoadGrants reads a user's whole grant set. It is one query, run once per authentication rather
// than once per authorization decision, because the set is small and the alternative is a database
// round trip inside every request on the estate view's thirty-second refetch.
func LoadGrants(ctx context.Context, pool *pgxpool.Pool, tenantID, userID string) ([]Grant, error) {
	// The rank comes from `roles`, which migration 000001 seeded. It is not a Go constant, because
	// a constant that disagrees with the table is a bug nothing would surface until somebody edited
	// one of the two.
	rows, err := pool.Query(ctx, `
		SELECT g.role_name, r.rank,
		       COALESCE(g.environment_id::text, ''), COALESCE(g.instance_id::text, '')
		FROM   role_grants g
		JOIN   roles r ON r.name = g.role_name
		WHERE  g.tenant_id = $1 AND g.user_id = $2`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("load role grants: %w", err)
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Role, &g.Rank, &g.EnvironmentID, &g.InstanceID); err != nil {
			return nil, fmt.Errorf("scan role grant: %w", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read role grants: %w", err)
	}
	return grants, nil
}

// -----------------------------------------------------------------------------------------------
// The bearer authenticator
// -----------------------------------------------------------------------------------------------

// BearerAuthenticator reads `Authorization: Bearer …`.
//
// The header is the only place a token is read from. Not a query parameter, not a form field: the
// access log records method, path and remote address for every request, and a credential in a URL
// would end the property that no log line can leak one (ADR-0033).
type BearerAuthenticator struct {
	tokens *TokenStore
}

// NewBearerAuthenticator builds one.
func NewBearerAuthenticator(tokens *TokenStore) *BearerAuthenticator {
	return &BearerAuthenticator{tokens: tokens}
}

// Authenticate implements Authenticator.
func (a *BearerAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	presented, ok := BearerFrom(r)
	if !ok {
		return Principal{}, ErrNoCredential
	}
	return a.tokens.Verify(ctx, presented)
}

// BearerFrom extracts a bearer credential from a request, if it carries one.
func BearerFrom(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(value) == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}
