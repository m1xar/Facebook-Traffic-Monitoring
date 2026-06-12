package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meta-tracking/internal/domain"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type contextKey string

const claimsKey contextKey = "claims"

type Claims struct {
	TokenType string      `json:"token_type"`
	UserID    int64       `json:"user_id"`
	Email     string      `json:"email"`
	Role      domain.Role `json:"role"`
	BuyerID   *int64      `json:"buyer_id,omitempty"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token"`
	TokenType             string    `json:"token_type"`
	AccessTokenExpiresIn  int64     `json:"access_token_expires_in"`
	RefreshTokenExpiresIn int64     `json:"refresh_token_expires_in"`
	RefreshTokenID        string    `json:"-"`
	RefreshTokenExpiresAt time.Time `json:"-"`
}

func ParseToken(secret, raw string) (*Claims, error) {
	return parseToken(secret, raw, "access")
}

func ParseRefreshToken(secret, raw string) (*Claims, error) {
	return parseToken(secret, raw, "refresh")
}

func parseToken(secret, raw, expectedType string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithLeeway(30*time.Second))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	if claims.TokenType != expectedType {
		return nil, fmt.Errorf("invalid token type")
	}
	if claims.UserID == 0 || claims.Email == "" {
		return nil, fmt.Errorf("user identity is required")
	}
	if claims.Role != domain.RoleAdmin && claims.Role != domain.RoleBuyer {
		return nil, fmt.Errorf("invalid role")
	}
	if claims.Role == domain.RoleBuyer && claims.BuyerID == nil {
		return nil, fmt.Errorf("buyer role requires buyer_id")
	}
	return claims, nil
}

func GenerateTokenPair(secret string, user domain.AppUser, now time.Time) (TokenPair, error) {
	accessTTL := 15 * time.Minute
	refreshTTL := 30 * 24 * time.Hour

	access, err := sign(secret, user, "access", uuid.NewString(), now, accessTTL)
	if err != nil {
		return TokenPair{}, err
	}
	refreshJTI := uuid.NewString()
	refresh, err := sign(secret, user, "refresh", refreshJTI, now, refreshTTL)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:           access,
		RefreshToken:          refresh,
		TokenType:             "Bearer",
		AccessTokenExpiresIn:  int64(accessTTL.Seconds()),
		RefreshTokenExpiresIn: int64(refreshTTL.Seconds()),
		RefreshTokenID:        refreshJTI,
		RefreshTokenExpiresAt: now.Add(refreshTTL),
	}, nil
}

func sign(secret string, user domain.AppUser, tokenType, jti string, now time.Time, ttl time.Duration) (string, error) {
	claims := Claims{
		TokenType: tokenType,
		UserID:    user.ID,
		Email:     user.Email,
		Role:      user.Role,
		BuyerID:   user.BuyerID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   fmt.Sprintf("%d", user.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func Middleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			claims, err := ParseToken(secret, strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
		})
	}
}

func FromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(claimsKey).(*Claims)
	return claims, ok
}

func RequireRole(roles ...domain.Role) func(http.Handler) http.Handler {
	allowed := make(map[domain.Role]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := FromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if _, ok := allowed[claims.Role]; !ok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
