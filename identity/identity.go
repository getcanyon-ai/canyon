// Package identity is the token Canyon hands an app about the signed-in person
// (design/PERMISSIONS.md): who they are, the app's roles, the app as audience — ES256, five
// minutes — and the key set the app verifies it with, from CANYON_IDENTITY_JWKS_URL.
package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const TTL = 5 * time.Minute // how long a token is good for

// Claims is the token's body. Mint sets IssuedAt and Expires.
type Claims struct {
	Issuer   string   `json:"iss"` // Canyon's public URL: CANYON_IDENTITY_ISSUER
	Subject  string   `json:"sub"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Audience string   `json:"aud"` // the app id: CANYON_IDENTITY_AUDIENCE
	Env      string   `json:"env"` // preview | prod
	IssuedAt int64    `json:"iat"`
	Expires  int64    `json:"exp"`
}

// Key signs tokens: a P-256 key whose ID, the kid every token carries, is its RFC 7638
// thumbprint, so a second key after a rotation is told apart without a counter.
type Key struct {
	ID   string
	priv *ecdsa.PrivateKey
}

// Generate makes a new key.
func Generate() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return newKey(priv)
}

// Parse reads a key Marshal wrote.
func Parse(der []byte) (*Key, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, errors.New("identity: the key is not P-256")
	}
	return newKey(priv)
}

func newKey(priv *ecdsa.PrivateKey) (*Key, error) {
	thumb, err := (&jose.JSONWebKey{Key: &priv.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, err
	}
	return &Key{ID: base64.RawURLEncoding.EncodeToString(thumb), priv: priv}, nil
}

// Marshal is the private key as PKCS #8 DER: what the server seals in the vault.
func (k *Key) Marshal() ([]byte, error) { return x509.MarshalPKCS8PrivateKey(k.priv) }

// Mint signs c, issued now and expiring TTL later, as a compact JWS whose header names the key.
// No roles is [], never null: an app compares strings in an array.
func (k *Key) Mint(c Claims) (string, error) {
	now := time.Now()
	c.IssuedAt, c.Expires = now.Unix(), now.Add(TTL).Unix()
	if c.Roles == nil {
		c.Roles = []string{}
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k.priv, KeyID: k.ID}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

// JWKS is the key set document an app fetches: the public half alone.
func (k *Key) JWKS() ([]byte, error) {
	return json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.priv.PublicKey, KeyID: k.ID, Algorithm: string(jose.ES256), Use: "sig"}}})
}
