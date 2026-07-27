package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

// envelopeVersion is the first byte of every stored blob. It exists so that a future change to the
// envelope layout can be rolled out without a migration that must decrypt everything at once.
const envelopeVersion byte = 1

// MasterKeySize is the required length, in bytes, of the AES-256 master key.
const MasterKeySize = 32

const (
	nonceSize = 12 // AES-GCM standard nonce size
	dekSize   = 32 // AES-256 data encryption key
)

// AESGCM is the MVP SecretsProvider: envelope encryption with AES-256-GCM, ciphertext at rest in
// PostgreSQL, master key supplied by the operator (ADR-0009).
//
// Envelope encryption — a fresh data key per secret, itself wrapped by the master key — costs
// almost nothing here and buys two things worth having: rotating the master key means rewrapping
// small data keys rather than re-encrypting every credential, and no single key ever encrypts
// enough data to approach AES-GCM's safety limits.
//
// The security of every stored credential reduces to the protection of the master key. That
// limitation is stated plainly in SECURITY.md rather than left to be discovered.
type AESGCM struct {
	store Store
	// masterAEAD wraps and unwraps data keys. It never touches secret plaintext directly.
	masterAEAD cipher.AEAD
	keyVersion int32
}

var _ Provider = (*AESGCM)(nil)

// NewAESGCM builds a provider from a raw 32-byte master key.
func NewAESGCM(store Store, masterKey []byte, keyVersion int32) (*AESGCM, error) {
	if store == nil {
		return nil, errors.New("aesgcm: store is required")
	}
	if len(masterKey) != MasterKeySize {
		return nil, fmt.Errorf("aesgcm: master key must be %d bytes, got %d", MasterKeySize, len(masterKey))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: create gcm: %w", err)
	}

	return &AESGCM{store: store, masterAEAD: aead, keyVersion: keyVersion}, nil
}

// LoadMasterKey resolves the master key from a file or an inline base64 value, in that order of
// preference. A file is preferred because an environment variable is readable by anything that can
// see the process.
//
// Both forms expect standard base64 of exactly 32 raw bytes.
func LoadMasterKey(inline, file string) ([]byte, error) {
	var encoded string
	switch {
	case file != "":
		// The path comes from operator configuration, which is exactly what it is for.
		raw, err := os.ReadFile(file) //nolint:gosec // G304: operator-configured key file path
		if err != nil {
			return nil, fmt.Errorf("read master key file %q: %w", file, err)
		}
		encoded = strings.TrimSpace(string(raw))
	case inline != "":
		encoded = strings.TrimSpace(inline)
	default:
		return nil, errors.New("no master key configured: set FLEETWARD_SECRETS_MASTER_KEY_FILE or FLEETWARD_SECRETS_MASTER_KEY")
	}

	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// The error deliberately does not echo the value, which may be a nearly-correct key.
		return nil, fmt.Errorf("master key is not valid base64: %w", err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("master key must decode to %d bytes, got %d", MasterKeySize, len(key))
	}
	return key, nil
}

// GenerateMasterKey returns a new base64-encoded master key, for use by `fleetward-cli keygen` and
// by the development stack's bootstrap.
func GenerateMasterKey() (string, error) {
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Name implements Provider.
func (a *AESGCM) Name() string { return "aesgcm" }

// Put encrypts plaintext under a fresh data key and stores the resulting envelope.
func (a *AESGCM) Put(ctx context.Context, ref Ref, plaintext []byte) error {
	if err := ref.Validate(); err != nil {
		return err
	}

	envelope, err := a.seal(ref, plaintext)
	if err != nil {
		return err
	}
	if err := a.store.PutSecret(ctx, ref, envelope, a.keyVersion); err != nil {
		return fmt.Errorf("store secret %s: %w", ref, err)
	}
	return nil
}

// Get retrieves and decrypts the secret at ref.
func (a *AESGCM) Get(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}

	envelope, _, err := a.store.GetSecret(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load secret %s: %w", ref, err)
	}

	plaintext, err := a.open(ref, envelope)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret %s: %w", ref, err)
	}
	return plaintext, nil
}

// Delete removes the secret at ref. Removing a secret that is already gone is not an error.
func (a *AESGCM) Delete(ctx context.Context, ref Ref) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := a.store.DeleteSecret(ctx, ref); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete secret %s: %w", ref, err)
	}
	return nil
}

// HealthCheck verifies the backing store is reachable.
func (a *AESGCM) HealthCheck(ctx context.Context) error {
	if err := a.store.PingSecrets(ctx); err != nil {
		return fmt.Errorf("secrets store unreachable: %w", err)
	}
	return nil
}

// Close implements Provider. The AES-GCM provider owns no resources of its own; the caller owns
// the store.
func (a *AESGCM) Close() error { return nil }

// Envelope layout, all fields concatenated:
//
//	 1 byte   version
//	12 bytes  nonce used to wrap the data key
//	 2 bytes  big-endian length of the wrapped data key
//	 n bytes  wrapped data key
//	12 bytes  nonce used to encrypt the payload
//	 m bytes  encrypted payload
//
// Both AES-GCM operations bind ref as additional authenticated data. That makes the ciphertext
// non-transferable: moving a row to another tenant or renaming it makes decryption fail loudly
// instead of yielding someone else's credential.
func (a *AESGCM) seal(ref Ref, plaintext []byte) ([]byte, error) {
	aad := []byte(ref.String())

	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}

	dekNonce := make([]byte, nonceSize)
	if _, err := rand.Read(dekNonce); err != nil {
		return nil, fmt.Errorf("generate key nonce: %w", err)
	}
	wrappedDEK := a.masterAEAD.Seal(nil, dekNonce, dek, aad)
	// A wrapped 32-byte key plus a GCM tag is 48 bytes; the guard exists so a future change to the
	// key size cannot silently truncate the length prefix and corrupt every subsequent secret.
	if len(wrappedDEK) > math.MaxUint16 {
		return nil, fmt.Errorf("wrapped data key is %d bytes, which exceeds the envelope format limit", len(wrappedDEK))
	}

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	dataNonce := make([]byte, nonceSize)
	if _, err := rand.Read(dataNonce); err != nil {
		return nil, fmt.Errorf("generate data nonce: %w", err)
	}
	ciphertext := dataAEAD.Seal(nil, dataNonce, plaintext, aad)

	out := make([]byte, 0, 1+nonceSize+2+len(wrappedDEK)+nonceSize+len(ciphertext))
	out = append(out, envelopeVersion)
	out = append(out, dekNonce...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrappedDEK))) //nolint:gosec // bounds-checked above
	out = append(out, wrappedDEK...)
	out = append(out, dataNonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func (a *AESGCM) open(ref Ref, envelope []byte) ([]byte, error) {
	aad := []byte(ref.String())

	// Every read below is bounds-checked before slicing: this data comes from a database row and
	// must be treated as untrusted input, not as something we know we wrote.
	if len(envelope) < 1 {
		return nil, errors.New("envelope is empty")
	}
	if envelope[0] != envelopeVersion {
		return nil, fmt.Errorf("unsupported envelope version %d", envelope[0])
	}
	rest := envelope[1:]

	if len(rest) < nonceSize+2 {
		return nil, errors.New("envelope truncated in header")
	}
	dekNonce := rest[:nonceSize]
	rest = rest[nonceSize:]

	wrappedLen := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if len(rest) < wrappedLen+nonceSize {
		return nil, errors.New("envelope truncated in wrapped key")
	}
	wrappedDEK := rest[:wrappedLen]
	rest = rest[wrappedLen:]

	dek, err := a.masterAEAD.Open(nil, dekNonce, wrappedDEK, aad)
	if err != nil {
		// This is the signal that either the master key is wrong or the row has been tampered
		// with. Both deserve the same blunt message and neither should reveal anything further.
		return nil, errors.New("unwrap data key failed: wrong master key or tampered ciphertext")
	}

	dataNonce := rest[:nonceSize]
	ciphertext := rest[nonceSize:]

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataAEAD.Open(nil, dataNonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("decrypt payload failed: tampered ciphertext")
	}
	return plaintext, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create data cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create data gcm: %w", err)
	}
	return aead, nil
}
