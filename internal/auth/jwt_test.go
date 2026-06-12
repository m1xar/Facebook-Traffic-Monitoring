package auth

import (
	"testing"
	"time"

	"meta-tracking/internal/domain"

	"github.com/golang-jwt/jwt/v5"
)

func TestParseTokenRequiresBuyerIDForBuyerRole(t *testing.T) {
	secret := "test-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TokenType: "access",
		UserID:    7,
		Email:     "buyer@example.com",
		Role:      domain.RoleBuyer,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	raw, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if _, err := ParseToken(secret, raw); err == nil {
		t.Fatalf("expected buyer token without buyer_id to fail")
	}
}

func TestParseTokenAcceptsAdminRole(t *testing.T) {
	secret := "test-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TokenType: "access",
		UserID:    1,
		Email:     "admin@example.com",
		Role:      domain.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	raw, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	claims, err := ParseToken(secret, raw)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if claims.Role != domain.RoleAdmin {
		t.Fatalf("unexpected role: %s", claims.Role)
	}
}

func TestGenerateTokenPairCreatesValidAccessAndRefresh(t *testing.T) {
	secret := "test-secret"
	user := domain.AppUser{
		ID:     1,
		Email:  "admin@example.com",
		Role:   domain.RoleAdmin,
		Status: "active",
	}

	pair, err := GenerateTokenPair(secret, user, time.Now())
	if err != nil {
		t.Fatalf("generate pair: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("tokens must be present: %+v", pair)
	}
	if _, err := ParseToken(secret, pair.AccessToken); err != nil {
		t.Fatalf("access token must parse: %v", err)
	}
	if _, err := ParseRefreshToken(secret, pair.RefreshToken); err != nil {
		t.Fatalf("refresh token must parse: %v", err)
	}
}
