package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestProvider(t *testing.T) (*AESGCM, *MemoryStore) {
	t.Helper()
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	store := NewMemoryStore()
	p, err := NewAESGCM(store, key, 1)
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	return p, store
}

func TestAESGCMRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		ref       Ref
		plaintext []byte
	}{
		{"short", Ref{TenantID: "t1", Name: "connection/a"}, []byte("hunter2")},
		{"empty", Ref{TenantID: "t1", Name: "connection/b"}, []byte{}},
		{"json credential", Ref{TenantID: "t1", Name: "connection/c"},
			[]byte(`{"username":"postgres","password":"p@ss:w/ord?#"}`)},
		{"binary", Ref{TenantID: "t2", Name: "connection/d"}, []byte{0x00, 0xff, 0x10, 0x00}},
		{"large", Ref{TenantID: "t2", Name: "connection/e"}, bytes.Repeat([]byte("x"), 1<<16)},
		{"unicode", Ref{TenantID: "tenant-ü", Name: "connection/ключ"}, []byte("пароль")},
	}

	p, _ := newTestProvider(t)
	ctx := context.Background()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.Put(ctx, tc.ref, tc.plaintext); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := p.Get(ctx, tc.ref)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, tc.plaintext) {
				t.Errorf("round trip mismatch: got %q, want %q", got, tc.plaintext)
			}
		})
	}
}

func TestAESGCMCiphertextIsNotPlaintext(t *testing.T) {
	p, store := newTestProvider(t)
	ctx := context.Background()
	ref := Ref{TenantID: "t1", Name: "connection/a"}
	secret := []byte("super-secret-password")

	if err := p.Put(ctx, ref, secret); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stored, _, err := store.GetSecret(ctx, ref)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if bytes.Contains(stored, secret) {
		t.Fatal("stored envelope contains the plaintext secret")
	}
	if stored[0] != envelopeVersion {
		t.Errorf("envelope version = %d, want %d", stored[0], envelopeVersion)
	}
}

func TestAESGCMNonceIsUniquePerWrite(t *testing.T) {
	p, store := newTestProvider(t)
	ctx := context.Background()
	ref := Ref{TenantID: "t1", Name: "connection/a"}
	secret := []byte("identical plaintext")

	if err := p.Put(ctx, ref, secret); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first, _, _ := store.GetSecret(ctx, ref)

	if err := p.Put(ctx, ref, secret); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	second, _, _ := store.GetSecret(ctx, ref)

	// Encrypting the same plaintext twice must not produce the same bytes. If it does, the nonce
	// or the data key is being reused, which is a catastrophic failure for AES-GCM.
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
}

func TestAESGCMRejectsTampering(t *testing.T) {
	ctx := context.Background()
	ref := Ref{TenantID: "t1", Name: "connection/a"}

	tests := []struct {
		name    string
		corrupt func(envelope []byte) []byte
	}{
		{"flip payload byte", func(e []byte) []byte {
			out := bytes.Clone(e)
			out[len(out)-1] ^= 0xff
			return out
		}},
		{"flip wrapped key byte", func(e []byte) []byte {
			out := bytes.Clone(e)
			out[20] ^= 0xff
			return out
		}},
		{"truncated", func(e []byte) []byte { return e[:len(e)/2] }},
		{"empty", func([]byte) []byte { return []byte{} }},
		{"wrong version", func(e []byte) []byte {
			out := bytes.Clone(e)
			out[0] = 0xfe
			return out
		}},
		{"header only", func(e []byte) []byte { return e[:3] }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, store := newTestProvider(t)
			if err := p.Put(ctx, ref, []byte("secret")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			envelope, _, _ := store.GetSecret(ctx, ref)
			store.Corrupt(ref, tc.corrupt(envelope))

			if _, err := p.Get(ctx, ref); err == nil {
				t.Fatal("expected decryption of tampered ciphertext to fail, got nil error")
			}
		})
	}
}

func TestAESGCMCiphertextIsBoundToRef(t *testing.T) {
	// A ciphertext copied to another tenant's row must not decrypt. The reference is bound in as
	// additional authenticated data precisely so that a database-level row swap fails loudly.
	p, store := newTestProvider(t)
	ctx := context.Background()

	victim := Ref{TenantID: "tenant-a", Name: "connection/x"}
	attacker := Ref{TenantID: "tenant-b", Name: "connection/x"}

	if err := p.Put(ctx, victim, []byte("tenant-a-password")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	envelope, _, _ := store.GetSecret(ctx, victim)

	if err := store.PutSecret(ctx, attacker, envelope, 1); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if _, err := p.Get(ctx, attacker); err == nil {
		t.Fatal("ciphertext moved to another tenant decrypted successfully; AAD binding is not working")
	}
}

func TestAESGCMWrongMasterKeyFails(t *testing.T) {
	ctx := context.Background()
	ref := Ref{TenantID: "t1", Name: "connection/a"}
	store := NewMemoryStore()

	keyA := make([]byte, MasterKeySize)
	keyB := make([]byte, MasterKeySize)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(keyB); err != nil {
		t.Fatalf("rand: %v", err)
	}

	writer, err := NewAESGCM(store, keyA, 1)
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	if err := writer.Put(ctx, ref, []byte("secret")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reader, err := NewAESGCM(store, keyB, 1)
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	if _, err := reader.Get(ctx, ref); err == nil {
		t.Fatal("decryption with the wrong master key succeeded")
	}
}

func TestAESGCMGetMissingReturnsErrNotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.Get(context.Background(), Ref{TenantID: "t1", Name: "nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestAESGCMDeleteIsIdempotent(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()
	ref := Ref{TenantID: "t1", Name: "connection/a"}

	// Deleting a secret that never existed must succeed: callers removing an instance should not
	// have to know whether it had stored credentials.
	if err := p.Delete(ctx, ref); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := p.Put(ctx, ref, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Delete(ctx, ref); err != nil {
		t.Fatalf("delete existing: %v", err)
	}
	if err := p.Delete(ctx, ref); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := p.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete got %v, want ErrNotFound", err)
	}
}

func TestRefValidate(t *testing.T) {
	tests := []struct {
		name    string
		ref     Ref
		wantErr bool
	}{
		{"valid", Ref{TenantID: "t", Name: "n"}, false},
		{"missing tenant", Ref{Name: "n"}, true},
		{"missing name", Ref{TenantID: "t"}, true},
		{"empty", Ref{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ref.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewAESGCMRejectsBadKeyLength(t *testing.T) {
	for _, size := range []int{0, 1, 16, 31, 33, 64} {
		key := make([]byte, size)
		if _, err := NewAESGCM(NewMemoryStore(), key, 1); err == nil {
			t.Errorf("key size %d: expected error, got nil", size)
		}
	}
}

func TestLoadMasterKey(t *testing.T) {
	valid, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master.key")
	// A trailing newline is what any sane editor or `echo` produces; it must not break loading.
	if err := os.WriteFile(keyFile, []byte(valid+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	shortFile := filepath.Join(dir, "short.key")
	if err := os.WriteFile(shortFile, []byte(base64.StdEncoding.EncodeToString([]byte("too short"))), 0o600); err != nil {
		t.Fatalf("write short key file: %v", err)
	}

	tests := []struct {
		name    string
		inline  string
		file    string
		wantErr bool
	}{
		{"from file", "", keyFile, false},
		{"from inline", valid, "", false},
		{"file wins over inline", "not-base64", keyFile, false},
		{"neither", "", "", true},
		{"inline not base64", "!!!not base64!!!", "", true},
		{"inline wrong length", base64.StdEncoding.EncodeToString([]byte("short")), "", true},
		{"file wrong length", "", shortFile, true},
		{"missing file", "", filepath.Join(dir, "absent.key"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := LoadMasterKey(tc.inline, tc.file)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadMasterKey: %v", err)
			}
			if len(key) != MasterKeySize {
				t.Fatalf("key length = %d, want %d", len(key), MasterKeySize)
			}
		})
	}
}

func TestLoadMasterKeyErrorsDoNotLeakKeyMaterial(t *testing.T) {
	// A near-miss key must not be echoed back in the error, since errors reach logs.
	almost := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcde")) // 31 bytes
	_, err := LoadMasterKey(almost, "")
	if err == nil {
		t.Fatal("expected error for 31-byte key")
	}
	if strings.Contains(err.Error(), almost) {
		t.Fatalf("error leaked key material: %v", err)
	}
}

func TestGenerateMasterKeyIsUsableAndDistinct(t *testing.T) {
	a, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	b, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	if a == b {
		t.Fatal("two generated master keys are identical")
	}

	key, err := LoadMasterKey(a, "")
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if _, err := NewAESGCM(NewMemoryStore(), key, 1); err != nil {
		t.Fatalf("generated key not usable: %v", err)
	}
}
