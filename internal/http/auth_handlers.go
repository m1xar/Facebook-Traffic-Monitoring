package apihttp

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"meta-tracking/internal/auth"
	"meta-tracking/internal/domain"
	"meta-tracking/internal/repository"

	"golang.org/x/crypto/bcrypt"
)

type accountRequest struct {
	Email    string      `json:"email"`
	Password string      `json:"password"`
	Role     domain.Role `json:"role"`
	BuyerID  *int64      `json:"buyer_id"`
}

type authResponse struct {
	User         domain.AppUser `json:"user"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	TokenType    string         `json:"token_type"`
	AccessTTL    int64          `json:"access_token_expires_in"`
	RefreshTTL   int64          `json:"refresh_token_expires_in"`
}

// createUser lets an admin create admin or buyer accounts.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateAccountInput(req.Email, req.Password, req.Role, req.BuyerID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	user, err := s.createAppUser(r, req)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) createAppUser(r *http.Request, req accountRequest) (domain.AppUser, error) {
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		return domain.AppUser{}, err
	}
	return s.store.CreateAppUser(r.Context(), repository.CreateAppUserParams{
		Email:        req.Email,
		PasswordHash: passwordHash,
		Role:         req.Role,
		BuyerID:      req.BuyerID,
	})
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	user, err := s.store.AppUserByEmail(r.Context(), req.Email)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if user == nil || user.Status != "active" {
		writeError(w, http.StatusUnauthorized, errors.New("invalid email or password"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid email or password"))
		return
	}
	resp, err := s.issueTokens(r, *user)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (s *Server) refreshToken(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	claims, err := auth.ParseRefreshToken(s.jwtSecret, req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid refresh token"))
		return
	}
	// Rotation: the token is single-use and revoked atomically; a replayed
	// token gets a 401 even before its JWT expiry.
	userID, ok, err := s.store.ConsumeRefreshToken(r.Context(), claims.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok || userID != claims.UserID {
		writeError(w, http.StatusUnauthorized, errors.New("invalid refresh token"))
		return
	}
	user, err := s.store.AppUserByID(r.Context(), userID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if user == nil || user.Status != "active" {
		writeError(w, http.StatusUnauthorized, errors.New("account is not active"))
		return
	}
	resp, err := s.issueTokens(r, *user)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) issueTokens(r *http.Request, user domain.AppUser) (authResponse, error) {
	tokens, err := auth.GenerateTokenPair(s.jwtSecret, user, time.Now().UTC())
	if err != nil {
		return authResponse{}, err
	}
	if err := s.store.CreateRefreshToken(r.Context(), tokens.RefreshTokenID, user.ID, tokens.RefreshTokenExpiresAt); err != nil {
		return authResponse{}, err
	}
	return authResponse{
		User:         user,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		TokenType:    tokens.TokenType,
		AccessTTL:    tokens.AccessTokenExpiresIn,
		RefreshTTL:   tokens.RefreshTokenExpiresIn,
	}, nil
}

func validateCredentials(email, password string) error {
	if _, err := mail.ParseAddress(strings.TrimSpace(email)); err != nil {
		return errors.New("valid email is required")
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

func validateAccountInput(email, password string, role domain.Role, buyerID *int64) error {
	if err := validateCredentials(email, password); err != nil {
		return err
	}
	if role == "" {
		return errors.New("role is required")
	}
	if role != domain.RoleAdmin && role != domain.RoleBuyer {
		return errors.New("role must be admin or buyer")
	}
	if role == domain.RoleBuyer && buyerID == nil {
		return errors.New("buyer role requires buyer_id")
	}
	return nil
}
