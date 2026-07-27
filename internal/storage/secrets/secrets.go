// Package secrets defines the SecretsProvider abstraction and its implementations (ADR-0009).
//
// Fleetward stores credentials for production databases. Two invariants hold everywhere in this
// package and in every caller:
//
//   - No API, log line, error message, or metric ever exposes a stored secret. Secrets are
//     write-only from the outside.
//   - Plugins never receive a provider. Core resolves a reference and passes materialized
//     credentials for the duration of a single RPC.
package secrets

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned when no secret exists at a reference.
var ErrNotFound = errors.New("secret not found")

// Ref locates one secret. Tenant is always part of the reference: it scopes lookups and is bound
// into the ciphertext as additional authenticated data, so a ciphertext moved between tenants
// fails to decrypt rather than silently succeeding.
type Ref struct {
	TenantID string
	// Name identifies the secret within the tenant, e.g. "connection/<uuid>".
	Name string
}

// String renders the reference. It contains no secret material and is safe to log.
func (r Ref) String() string { return r.TenantID + "/" + r.Name }

// Validate reports whether the reference is usable.
func (r Ref) Validate() error {
	if r.TenantID == "" {
		return fmt.Errorf("secret ref: tenant_id is required")
	}
	if r.Name == "" {
		return fmt.Errorf("secret ref: name is required")
	}
	return nil
}

// Provider stores and retrieves secret material. Implementations must be safe for concurrent use.
//
// The interface deals in opaque bytes rather than a credential struct so that a provider never has
// to understand what it is protecting, and so that adding a field to a credential does not touch
// any provider.
type Provider interface {
	// Name identifies the implementation for logging and health reporting, e.g. "aesgcm".
	Name() string

	// Put stores plaintext at ref, replacing anything already there.
	Put(ctx context.Context, ref Ref, plaintext []byte) error

	// Get retrieves the plaintext at ref, returning ErrNotFound if there is none.
	Get(ctx context.Context, ref Ref) ([]byte, error)

	// Delete removes the secret at ref. Deleting a secret that does not exist is not an error:
	// callers deleting an instance should not have to care whether its credentials survived.
	Delete(ctx context.Context, ref Ref) error

	// HealthCheck reports whether the provider can currently serve requests.
	HealthCheck(ctx context.Context) error

	// Close releases any resources held by the provider.
	Close() error
}

// Store is the persistence backend an encrypting Provider writes ciphertext to. Separating it from
// Provider keeps the cryptography testable without a database, and keeps the database layer
// unaware of what it is storing.
type Store interface {
	PutSecret(ctx context.Context, ref Ref, ciphertext []byte, keyVersion int32) error
	GetSecret(ctx context.Context, ref Ref) (ciphertext []byte, keyVersion int32, err error)
	DeleteSecret(ctx context.Context, ref Ref) error
	PingSecrets(ctx context.Context) error
}
