// Package authclient validates the short-lived JWT that sapienza-core issues to
// let a user operate a product's API. The core is the sole identity issuer;
// products only verify. HS256 with a shared secret keeps the boundary simple
// for a single-org platform.
package authclient

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the platform claims carried by the product JWT.
type Claims struct {
	UserID   uuid.UUID `json:"uid"`
	TenantID uuid.UUID `json:"tid"`
	Produto  string    `json:"produto,omitempty"`
	Role     string    `json:"role,omitempty"`
	jwt.RegisteredClaims
}

// Verifier validates product JWTs signed by the core.
type Verifier struct {
	secret []byte
	issuer string
}

// NewVerifier builds a verifier with the shared secret and expected issuer
// (e.g. "sapienza-core"). issuer may be empty to skip the issuer check.
func NewVerifier(secret []byte, issuer string) *Verifier {
	return &Verifier{secret: secret, issuer: issuer}
}

// Verify parses and validates a token string, returning its claims. It enforces
// HS256, expiry, and (when set) the expected issuer.
func (v *Verifier) Verify(tokenString string) (*Claims, error) {
	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"HS256"})}
	if v.issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.issuer))
	}
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		return v.secret, nil
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("verify product jwt: %w", err)
	}
	return claims, nil
}
