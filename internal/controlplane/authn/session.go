package authn

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionCookieName is the cookie a browser carries. `__Host-` is not used because the development
// stack serves over plain HTTP, and a prefix the browser refuses in development is a prefix nobody
// would find out was wrong until production.
const SessionCookieName = "fleetward_session"

// Sessions mints and verifies the cookie a browser holds instead of a token.
//
// This exists so that the UI never holds a credential in JavaScript. A token in localStorage or
// sessionStorage is readable by any script that gets onto the page; an HttpOnly cookie is not, and
// the difference is the whole reason the UI exchanges one for the other rather than simply sending
// the token it was given (ADR-0033).
//
// The session is a signed statement, not a row: there is no server-side session table to grow, to
// clean up, or to fail to clean up. It carries the user id and an expiry, and the signature is what
// makes it unforgeable. Revoking a session before it expires therefore means revoking the user's
// token, which is stated plainly in docs/ops/authorization.md rather than left to be discovered.
type Sessions struct {
	pool   *pgxpool.Pool
	key    []byte
	ttl    time.Duration
	secure bool
}

// NewSessions builds the session issuer.
//
// A nil or empty key is a programming error rather than a fallback: signing with a zero key would
// let anybody mint a session for any user, and defaulting quietly is how that ships.
func NewSessions(pool *pgxpool.Pool, key []byte, ttl time.Duration, secure bool) (*Sessions, error) {
	if len(key) < 32 {
		return nil, errors.New("session signing key must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("session TTL must be positive")
	}
	return &Sessions{pool: pool, key: key, ttl: ttl, secure: secure}, nil
}

// GenerateSessionKey produces a signing key.
//
// The control plane calls this at startup when no key is configured, which means restarting it
// signs everybody out. That is the right default for a single node: it costs one sign-in and it
// removes a mandatory secret from the quickstart. An installation that runs more than one replica,
// or that would rather not sign everyone out on a deploy, configures
// FLEETWARD_AUTH_SESSION_KEY_FILE — and if it does not, the two replicas simply reject each other's
// cookies, which presents as "signed out again" rather than as anything unsafe.
func GenerateSessionKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate session key: %w", err)
	}
	return key, nil
}

// Issue builds the cookie for a principal.
func (s *Sessions) Issue(p Principal) (*http.Cookie, error) {
	if p.UserID == "" {
		return nil, errors.New("only a user principal can hold a session")
	}
	payload, err := json.Marshal(sessionPayload{
		UserID:   p.UserID,
		TenantID: p.TenantID,
		Expires:  time.Now().Add(s.ttl).Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode session: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(payload)
	// gosec G124 flags Secure being a variable rather than a literal true, and it is right to look.
	// It is operator configuration, not a default: the cookie is Secure exactly when the control
	// plane is serving TLS. Hardcoding true would make the development stack — plain HTTP, by its
	// own declaration — silently fail to keep anybody signed in, which is a worse outcome than an
	// operator who has already chosen to serve without TLS also getting a cookie without Secure.
	// HttpOnly and SameSite=Strict are unconditional, and they are the two doing the work here.
	return &http.Cookie{ //nolint:gosec // G124: Secure follows the server's own TLS setting
		Name:  SessionCookieName,
		Value: encoded + "." + s.sign(encoded),
		Path:  "/",
		// HttpOnly is the point of the whole mechanism: script on the page cannot read this.
		HttpOnly: true,
		// Secure follows whether the server itself is serving TLS. Setting it unconditionally would
		// make the development stack silently fail to keep anybody signed in.
		Secure: s.secure,
		// Strict rather than Lax. The UI performs no mutations in B6, so Strict is sufficient CSRF
		// defence on its own; the first mutating screen needs a double-submit token as well, and
		// that is written down in docs/ops/authorization.md.
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.ttl.Seconds()),
	}, nil
}

// Clear builds the cookie that ends a session.
func (s *Sessions) Clear() *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124: as in Issue — Secure follows the server's TLS setting
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
}

type sessionPayload struct {
	UserID   string `json:"u"`
	TenantID string `json:"t"`
	Expires  int64  `json:"e"`
}

func (s *Sessions) sign(encoded string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Authenticate implements Authenticator: it reads the cookie, checks the signature and the expiry,
// and loads the user it names.
func (s *Sessions) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Principal{}, ErrNoCredential
	}

	encoded, signature, found := strings.Cut(cookie.Value, ".")
	if !found {
		return Principal{}, ErrInvalidCredential
	}
	if !hmac.Equal([]byte(signature), []byte(s.sign(encoded))) {
		return Principal{}, ErrInvalidCredential
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	var payload sessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Principal{}, ErrInvalidCredential
	}
	if time.Now().Unix() > payload.Expires {
		return Principal{}, ErrInvalidCredential
	}

	// The grants are re-read rather than carried in the cookie. A cookie that carried them would
	// keep granting whatever it was signed with until it expired, so revoking a role would not take
	// effect for the length of a session — which is hours.
	var email, displayName string
	var isActive bool
	err = s.pool.QueryRow(ctx,
		`SELECT email, display_name, is_active FROM users WHERE id = $1 AND tenant_id = $2`,
		payload.UserID, payload.TenantID).Scan(&email, &displayName, &isActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, fmt.Errorf("load session user: %w", err)
	}
	if !isActive {
		return Principal{}, ErrInvalidCredential
	}

	grants, err := LoadGrants(ctx, s.pool, payload.TenantID, payload.UserID)
	if err != nil {
		return Principal{}, err
	}

	return Principal{
		Kind:        KindUser,
		UserID:      payload.UserID,
		Actor:       email,
		DisplayName: displayName,
		Email:       email,
		TenantID:    payload.TenantID,
		Grants:      grants,
	}, nil
}

// LoadSessionKey reads the configured signing key, or generates one.
//
// The inline form is accepted and the file form is preferred, for the reason `fleetward-cli keygen`
// already gives about the secrets master key: anything that can read the process environment can
// read an environment variable.
func LoadSessionKey(inline, file string) ([]byte, bool, error) {
	var encoded string
	switch {
	case file != "":
		raw, err := readTrimmedFile(file)
		if err != nil {
			return nil, false, err
		}
		encoded = raw
	case inline != "":
		encoded = strings.TrimSpace(inline)
	default:
		key, err := GenerateSessionKey()
		return key, false, err
	}

	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Deliberately does not echo the value, which may be a nearly-correct key.
		return nil, false, fmt.Errorf("session key is not valid base64: %w", err)
	}
	if len(key) < 32 {
		return nil, false, fmt.Errorf("session key must decode to at least 32 bytes, got %d", len(key))
	}
	return key, true, nil
}
