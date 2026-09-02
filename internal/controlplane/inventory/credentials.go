package inventory

import (
	"encoding/json"
	"errors"
	"fmt"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// Credentials are split across two stores, and the split is the whole point (ADR-0009).
//
// Non-secret connection fields — username, database, TLS flags, engine options — live in the
// `connections` table, where an operator can read and audit them. Only material that would let
// somebody log in goes to the SecretsProvider, encrypted, under `connection/<connection-uuid>`.
// Nothing else may store either half.
//
// The client certificate travels with its private key rather than with the other TLS settings.
// A certificate on its own is not secret, but a half-configured mutual-TLS connection is a
// confusing failure, and keeping the pair together makes it impossible to store one without the
// other.

// credentialSecret is the JSON document held by the SecretsProvider for one connection. It is
// decrypted only to build a Credentials message for a single plugin call and is never logged,
// returned by an API, or written anywhere else.
type credentialSecret struct {
	Password      string `json:"password,omitempty"`
	ClientCertPEM []byte `json:"client_cert_pem,omitempty"`
	ClientKeyPEM  []byte `json:"client_key_pem,omitempty"`
}

// connectionOptions is the document stored in `connections.options`. Engine options are kept
// separate from Fleetward's own TLS fields so that nothing we add can ever collide with a key the
// plugin passes through to its driver.
type connectionOptions struct {
	Engine map[string]string `json:"engine,omitempty"`
	TLS    *tlsOptions       `json:"tls,omitempty"`
	// Share is where this instance's backup files are written and where this control plane sees
	// the same directory (ADR-0026). It is not a secret — it is a path — so it lives here beside
	// the other non-secret half of a connection rather than in the secret store.
	Share *sharedDirOptions `json:"shared_directory,omitempty"`
}

// sharedDirOptions is the stored form of one directory under the two names its two users know it
// by. Only an engine whose backup tooling writes a file rather than a stream needs one.
type sharedDirOptions struct {
	EnginePath string `json:"engine_path,omitempty"`
	LocalPath  string `json:"local_path,omitempty"`
}

// tlsOptions holds the non-secret half of a connection's TLS configuration. Whether TLS is on at
// all lives in the `tls_enabled` column instead, because that is the one field an operator query
// or a compliance report cares about.
type tlsOptions struct {
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	ServerName         string `json:"server_name,omitempty"`
	CAPEM              []byte `json:"ca_pem,omitempty"`
}

// marshalCredentialSecret extracts the secret half of a connection spec.
func marshalCredentialSecret(spec *fwv1.ConnectionSpec) ([]byte, error) {
	payload := credentialSecret{
		Password:      spec.GetPassword(),
		ClientCertPEM: spec.GetTls().GetClientCertPem(),
		ClientKeyPEM:  spec.GetTls().GetClientKeyPem(),
	}
	// G117 flags marshaling a struct with a password field, which is precisely what this function is
	// for: the output goes straight to the SecretsProvider and is encrypted before it is stored.
	encoded, err := json.Marshal(payload) //nolint:gosec // G117: this payload is what the provider encrypts
	if err != nil {
		// The value cannot appear in the error: it is the credential.
		return nil, errors.New("inventory: encode credentials")
	}
	return encoded, nil
}

// marshalConnectionOptions extracts the non-secret half of a connection spec.
func marshalConnectionOptions(spec *fwv1.ConnectionSpec) ([]byte, error) {
	opts := connectionOptions{Engine: spec.GetOptions()}
	if share := spec.GetSharedDirectory(); share.GetEnginePath() != "" || share.GetLocalPath() != "" {
		opts.Share = &sharedDirOptions{
			EnginePath: share.GetEnginePath(),
			LocalPath:  share.GetLocalPath(),
		}
	}
	if tls := spec.GetTls(); tls.GetEnabled() {
		opts.TLS = &tlsOptions{
			InsecureSkipVerify: tls.GetInsecureSkipVerify(),
			ServerName:         tls.GetServerName(),
			CAPEM:              tls.GetCaPem(),
		}
	}

	encoded, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("inventory: encode connection options: %w", err)
	}
	return encoded, nil
}

// unmarshalConnectionOptions reads the document back. An empty column is a valid, option-free
// connection.
func unmarshalConnectionOptions(raw []byte) (*connectionOptions, error) {
	opts := &connectionOptions{}
	if len(raw) == 0 {
		return opts, nil
	}
	if err := json.Unmarshal(raw, opts); err != nil {
		return nil, fmt.Errorf("inventory: decode connection options: %w", err)
	}
	return opts, nil
}

// sharedDirectory reassembles the contract's message, or nil when this connection has no such
// directory. Absent rather than empty-and-present: a plugin reads a nil block as "this engine
// streams", which is the truth for every engine but the few that hand over a file.
func (o *connectionOptions) sharedDirectory() *fwv1.SharedDirectory {
	if o.Share == nil {
		return nil
	}
	return &fwv1.SharedDirectory{
		EnginePath: o.Share.EnginePath,
		LocalPath:  o.Share.LocalPath,
	}
}

// tlsSettings reassembles the contract's TLS message from both halves for one plugin call.
func (o *connectionOptions) tlsSettings(enabled bool, payload *credentialSecret) *fwv1.TLSSettings {
	if !enabled {
		// Absent rather than disabled-and-empty: the plugin contract treats a nil TLS block as "no
		// TLS", and sending an empty one would look like a configuration that was lost in transit.
		return nil
	}

	settings := &fwv1.TLSSettings{Enabled: true}
	if o.TLS != nil {
		settings.InsecureSkipVerify = o.TLS.InsecureSkipVerify
		settings.ServerName = o.TLS.ServerName
		settings.CaPem = o.TLS.CAPEM
	}
	if payload != nil {
		settings.ClientCertPem = payload.ClientCertPEM
		settings.ClientKeyPem = payload.ClientKeyPEM
	}
	return settings
}
