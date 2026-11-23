// Package agentkey holds the credential of an agent: an SSH key pair.
//
// The server holds the public key of every agent it knows, and the agent
// holds the private one. Every request of an agent carries a signature made
// with the private key, which the server verifies with the public key before
// the request runs. The identity of the caller is the agent the key belongs
// to, never a name the request carries (doc/graft/PLAN14.md).
//
// The signature covers the method of the call, the time of the request, a
// nonce that makes a captured request useless, and the hash of the request
// itself. The server refuses a request whose time is outside the validity
// window, whose nonce it has already seen, or whose signature does not match.
package agentkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/metadata"
)

// The metadata keys of the signature of a request. gRPC metadata keys are
// lowercase.
const (
	// KeyMetadata holds the fingerprint of the public key that signed the
	// request, in the format of ssh-keygen ("SHA256:...").
	KeyMetadata = "graft-key"
	// TimestampMetadata holds the time of the request, in seconds since the
	// epoch.
	TimestampMetadata = "graft-timestamp"
	// NonceMetadata holds the nonce of the request: 16 random bytes in hex.
	NonceMetadata = "graft-nonce"
	// SignatureMetadata holds the signature of the request, in the SSH wire
	// format, base64-encoded.
	SignatureMetadata = "graft-signature"
)

// PayloadVersion is the version of the signed payload. It is the first line
// of the payload, so that a signature of another version never verifies.
const PayloadVersion = "graft-ssh-v1"

// NonceBytes is the number of random bytes of a nonce.
const NonceBytes = 16

// Fingerprint returns the fingerprint of the public key, in the format of
// ssh-keygen: "SHA256:" and the base64 of the SHA-256 of the key, without
// the padding.
func Fingerprint(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// ParsePublicKey parses the public key of an agent: an OpenSSH public key
// ("ssh-ed25519 AAAA... comment"), as ssh-keygen writes it.
func ParsePublicKey(text []byte) (ssh.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey(text)
	if err != nil {
		return nil, fmt.Errorf("parse the public key: %w", err)
	}
	return pub, nil
}

// ParsePrivateKey parses the private key of an agent: a PEM private key, in
// the OpenSSH format or in the PKCS#1 and PKCS#8 formats.
func ParsePrivateKey(text []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(text)
	if err != nil {
		return nil, fmt.Errorf("parse the private key: %w", err)
	}
	return signer, nil
}

// randReader is the source of the random bytes of a nonce. It is a variable
// so that a test can make the read fail: a signer that cannot draw a nonce
// must refuse to send the request rather than send it unsigned.
var randReader io.Reader = rand.Reader

// Nonce returns a new nonce: random bytes in hex.
func Nonce() (string, error) {
	buf := make([]byte, NonceBytes)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		return "", fmt.Errorf("read the random bytes of the nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Signature is the signature of a request as it travels in the metadata of a
// gRPC call.
type Signature struct {
	// Fingerprint is the fingerprint of the public key that signed the
	// request.
	Fingerprint string
	// Timestamp is the time of the request, in seconds since the epoch.
	Timestamp int64
	// Nonce is the nonce of the request.
	Nonce string
	// Value is the signature itself, base64-encoded.
	Value string
}

// SignatureFromMetadata returns the signature the request carrying ctx
// presents. It returns nil when the request carries no signature at all, and
// an error when it carries an incomplete one: a request that presents a
// signature must present all of it, so that a broken signature is never
// silently ignored.
func SignatureFromMetadata(ctx context.Context) (*Signature, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, nil
	}

	fingerprint := firstValue(md, KeyMetadata)
	timestamp := firstValue(md, TimestampMetadata)
	nonce := firstValue(md, NonceMetadata)
	value := firstValue(md, SignatureMetadata)

	if fingerprint == "" && timestamp == "" && nonce == "" && value == "" {
		return nil, nil
	}

	if fingerprint == "" || timestamp == "" || nonce == "" || value == "" {
		return nil, fmt.Errorf("the signature of the request is incomplete")
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("the timestamp of the signature is invalid")
	}

	return &Signature{
		Fingerprint: fingerprint,
		Timestamp:   seconds,
		Nonce:       nonce,
		Value:       value,
	}, nil
}

// firstValue returns the first value of the metadata key, or the empty
// string when the key is absent.
func firstValue(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// Payload returns the bytes an agent signs: the version, the time of the
// request, the nonce, the method of the call and the hash of the request.
//
// The hash of the request is empty for a streaming call, whose message the
// caller does not know when it signs. The server builds the same payload from
// the request it received, so that a signature that does not match the
// request is refused.
func Payload(timestamp int64, nonce, method, bodyHash string) []byte {
	var b strings.Builder
	b.WriteString(PayloadVersion)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(timestamp, 10))
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(bodyHash)
	b.WriteByte('\n')
	return []byte(b.String())
}

// Sign signs the payload with the private key of the agent. The signature is
// in the SSH wire format, base64-encoded, as it travels in the metadata of
// the request.
func Sign(signer ssh.Signer, payload []byte) (string, error) {
	sig, err := signer.Sign(rand.Reader, payload)
	if err != nil {
		return "", fmt.Errorf("sign the request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ssh.Marshal(sig)), nil
}

// Verify verifies the signature of the payload with the public key of the
// agent. The signature is the base64 of the SSH wire format, as it travels in
// the metadata of the request.
func Verify(pub ssh.PublicKey, payload []byte, signature string) error {
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("decode the signature: %w", err)
	}

	var sig ssh.Signature
	if err := ssh.Unmarshal(raw, &sig); err != nil {
		return fmt.Errorf("parse the signature: %w", err)
	}

	if err := pub.Verify(payload, &sig); err != nil {
		return fmt.Errorf("verify the signature: %w", err)
	}
	return nil
}

// formatTimestamp returns the timestamp as it travels in the metadata.
func formatTimestamp(timestamp int64) string {
	return strconv.FormatInt(timestamp, 10)
}
