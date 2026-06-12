package apihttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"meta-tracking/internal/auth"
	"meta-tracking/internal/crypto"
	"meta-tracking/internal/domain"
	"meta-tracking/internal/meta"
	"meta-tracking/internal/repository"
	"meta-tracking/internal/worker"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const maxRequestBody = 1 << 20 // 1 MiB

type Server struct {
	router       chi.Router
	store        *repository.Store
	metaClient   *meta.Client
	tokenCipher  *crypto.TokenCipher
	jwtSecret    string
	asynqClient  *asynq.Client
	syncInterval time.Duration
}

// asynqClient may be nil; then the first sync waits for the scheduler.
func NewServer(store *repository.Store, metaClient *meta.Client, tokenCipher *crypto.TokenCipher, jwtSecret string, asynqClient *asynq.Client, syncInterval time.Duration) *Server {
	s := &Server{
		router:       chi.NewRouter(),
		store:        store,
		metaClient:   metaClient,
		tokenCipher:  tokenCipher,
		jwtSecret:    jwtSecret,
		asynqClient:  asynqClient,
		syncInterval: syncInterval,
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.router
}

func (s *Server) routes() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.RealIP)
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.Timeout(60 * time.Second))

	s.registerDocs()

	s.router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// The OAuth callback is hit by Facebook's browser redirect, which carries no
	// Authorization header, so it sits outside the JWT-protected /api group and
	// authenticates the caller through the signed state parameter instead.
	s.router.Get("/oauth/facebook/callback", s.oauthCallback)

	s.router.Route("/api/auth", func(r chi.Router) {
		r.Post("/register", s.registerAccount)
		r.Post("/login", s.login)
		r.Post("/refresh", s.refreshToken)
	})

	s.router.Route("/api", func(r chi.Router) {
		r.Use(auth.Middleware(s.jwtSecret))

		admin := r.With(auth.RequireRole(domain.RoleAdmin))
		anyRole := r.With(auth.RequireRole(domain.RoleAdmin, domain.RoleBuyer))

		admin.Post("/users", s.createUser)

		admin.Post("/buyers", s.createBuyer)
		admin.Get("/buyers", s.listBuyers)

		admin.Get("/fb-profiles/oauth/start", s.oauthStart)
		admin.Get("/fb-profiles", s.listFBProfiles)
		admin.Post("/fb-profiles/{id}/resync", s.resyncFBProfile)

		anyRole.Get("/ad-accounts", s.listAdAccounts)
		anyRole.Get("/ad-accounts/{id}/snapshots", s.accountSnapshots)
		admin.Get("/ad-accounts/{id}/assignments", s.accountAssignments)
		admin.Post("/ad-accounts/{id}/assign", s.assignBuyer)
		admin.Post("/ad-accounts/{id}/unassign", s.unassignBuyer)
		admin.Patch("/ad-accounts/{id}", s.updateAdAccount)

		anyRole.Get("/snapshots", s.listSnapshots)

		admin.Get("/alerts", s.listAlerts)
	})
}

type createBuyerRequest struct {
	DisplayName      string  `json:"display_name"`
	Email            string  `json:"email"`
	Password         string  `json:"password"`
	TelegramUsername *string `json:"telegram_username"`
	CRMExternalID    *string `json:"crm_external_id"`
}

// createBuyer creates the buyer together with their CRM login in one step, so
// the admin hands out ready-to-use credentials.
func (s *Server) createBuyer(w http.ResponseWriter, r *http.Request) {
	var req createBuyerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, errors.New("display_name is required"))
		return
	}
	if err := validateCredentials(req.Email, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		s.internalError(w, err)
		return
	}
	buyer, user, err := s.store.CreateBuyerWithUser(r.Context(), repository.CreateBuyerWithUserParams{
		DisplayName:      req.DisplayName,
		Email:            req.Email,
		PasswordHash:     passwordHash,
		TelegramUsername: req.TelegramUsername,
		CRMExternalID:    req.CRMExternalID,
	})
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("buyer with this email or crm_external_id already exists"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"buyer": buyer, "user": user})
}

func (s *Server) listBuyers(w http.ResponseWriter, r *http.Request) {
	buyers, err := s.store.ListBuyers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": buyers})
}

func (s *Server) listFBProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.store.ListFBProfiles(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": profiles})
}

// resyncFBProfile re-pulls the profile's ad accounts with the stored token and
// merges them into the tracking pool (new accounts are added, known ones are
// refreshed, nothing is duplicated), then queues an immediate stats sync.
func (s *Server) resyncFBProfile(w http.ResponseWriter, r *http.Request) {
	profileID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid profile id"))
		return
	}
	profile, err := s.store.FBProfileByID(r.Context(), profileID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if profile == nil {
		writeError(w, http.StatusNotFound, errors.New("fb profile not found"))
		return
	}
	token, err := s.tokenCipher.Decrypt(profile.AccessTokenCiphertext)
	if err != nil {
		s.internalError(w, err)
		return
	}
	accounts, err := s.metaClient.AdAccounts(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := s.store.UpsertAdAccountsForProfile(r.Context(), profile.ID, accounts); err != nil {
		s.internalError(w, err)
		return
	}
	if s.asynqClient != nil {
		if err := worker.EnqueueSyncProfile(r.Context(), s.asynqClient, profile.ID, 0, s.syncInterval); err != nil {
			log.Printf("enqueue sync for profile %d: %v", profile.ID, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "resynced", "ad_accounts": accounts})
}

// oauthStateTTL bounds how long a started OAuth flow stays valid; the signed
// state must be redeemed at the callback within this window.
const oauthStateTTL = 10 * time.Minute

// oauthState is the signed payload carried through the Facebook redirect. It
// lets the unauthenticated callback recover which admin started the flow, and
// its signature prevents forged callbacks (CSRF).
type oauthState struct {
	UserID int64 `json:"user_id"`
	jwt.RegisteredClaims
}

func (s *Server) signOAuthState(claims *auth.Claims, now time.Time) (string, error) {
	state := oauthState{
		UserID: claims.UserID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   "fb_oauth_state",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(oauthStateTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, state).SignedString([]byte(s.jwtSecret))
}

func (s *Server) parseOAuthState(raw string) (*oauthState, error) {
	state := &oauthState{}
	token, err := jwt.ParseWithClaims(raw, state, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(s.jwtSecret), nil
	}, jwt.WithLeeway(30*time.Second))
	if err != nil {
		return nil, err
	}
	if !token.Valid || state.Subject != "fb_oauth_state" {
		return nil, fmt.Errorf("invalid oauth state")
	}
	return state, nil
}

// oauthStart returns the Facebook Login URL the admin should be redirected to.
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	state, err := s.signOAuthState(claims, time.Now())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": s.metaClient.AuthCodeURL(state)})
}

// oauthCallback completes the Facebook Login flow: it validates the signed
// state, exchanges the code for a long-lived token, stores the profile, and
// imports its ad accounts into the tracking pool.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		desc := r.URL.Query().Get("error_description")
		if desc == "" {
			desc = oauthErr
		}
		writeError(w, http.StatusBadRequest, fmt.Errorf("facebook login was not completed: %s", desc))
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, errors.New("code is required"))
		return
	}
	if _, err := s.parseOAuthState(r.URL.Query().Get("state")); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid or expired state"))
		return
	}

	shortToken, _, err := s.metaClient.ExchangeCode(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	longToken, expiresIn, err := s.metaClient.ExchangeLongLivedToken(r.Context(), shortToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	me, err := s.metaClient.Me(r.Context(), longToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	ciphertext, err := s.tokenCipher.Encrypt(longToken)
	if err != nil {
		s.internalError(w, err)
		return
	}

	var expiresAt *time.Time
	if expiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
		expiresAt = &t
	}
	profile, err := s.store.UpsertFBProfile(r.Context(), me.ID, me.Name, ciphertext, expiresAt)
	if err != nil {
		s.internalError(w, err)
		return
	}

	accounts, err := s.metaClient.AdAccounts(r.Context(), longToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := s.store.UpsertAdAccountsForProfile(r.Context(), profile.ID, accounts); err != nil {
		s.internalError(w, err)
		return
	}

	// First sync runs right away; afterwards the jittered scheduler owns the
	// cadence. A failed enqueue is not worth failing the connect.
	if s.asynqClient != nil {
		if err := worker.EnqueueSyncProfile(r.Context(), s.asynqClient, profile.ID, 0, s.syncInterval); err != nil {
			log.Printf("enqueue first sync for profile %d: %v", profile.ID, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"profile":     profile,
		"ad_accounts": accounts,
	})
}

func (s *Server) listAdAccounts(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	var buyerID *int64
	if claims.Role == domain.RoleBuyer {
		buyerID = claims.BuyerID
	}
	accounts, err := s.store.ListAdAccounts(r.Context(), buyerID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": accounts})
}

// accountSnapshots returns the raw snapshots of one ad account. Buyers only
// see snapshots captured while the account was assigned to them.
func (s *Server) accountSnapshots(w http.ResponseWriter, r *http.Request) {
	adAccountID := chi.URLParam(r, "id")
	from, to, err := parseTimeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	query := repository.SnapshotQuery{AdAccountID: &adAccountID, From: from, To: to}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleBuyer {
		query.BuyerID = claims.BuyerID
	}

	snapshots, err := s.store.Snapshots(r.Context(), query)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": snapshots})
}

// listSnapshots returns snapshots across accounts. Admins can filter by
// buyer_id and/or ad_account_id; buyers are always restricted to their own
// ownership intervals.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	from, to, err := parseTimeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	query := repository.SnapshotQuery{From: from, To: to}
	if raw := r.URL.Query().Get("ad_account_id"); raw != "" {
		query.AdAccountID = &raw
	}
	if raw := r.URL.Query().Get("buyer_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid buyer_id"))
			return
		}
		query.BuyerID = &parsed
	}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleBuyer {
		query.BuyerID = claims.BuyerID
	}

	snapshots, err := s.store.Snapshots(r.Context(), query)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": snapshots})
}

func (s *Server) accountAssignments(w http.ResponseWriter, r *http.Request) {
	adAccountID := chi.URLParam(r, "id")
	assignments, err := s.store.AccountAssignments(r.Context(), adAccountID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": assignments})
}

type assignBuyerRequest struct {
	BuyerID int64 `json:"buyer_id"`
}

func (s *Server) assignBuyer(w http.ResponseWriter, r *http.Request) {
	var req assignBuyerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.BuyerID == 0 {
		writeError(w, http.StatusBadRequest, errors.New("buyer_id is required"))
		return
	}
	adAccountID := chi.URLParam(r, "id")
	if adAccountID == "" {
		writeError(w, http.StatusBadRequest, errors.New("ad account id is required"))
		return
	}
	if err := s.store.AssignBuyer(r.Context(), adAccountID, req.BuyerID, time.Now().UTC()); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "assigned"})
}

func (s *Server) unassignBuyer(w http.ResponseWriter, r *http.Request) {
	adAccountID := chi.URLParam(r, "id")
	if adAccountID == "" {
		writeError(w, http.StatusBadRequest, errors.New("ad account id is required"))
		return
	}
	if err := s.store.UnassignBuyer(r.Context(), adAccountID, time.Now().UTC()); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unassigned"})
}

type updateAdAccountRequest struct {
	IsTracked *bool `json:"is_tracked"`
}

func (s *Server) updateAdAccount(w http.ResponseWriter, r *http.Request) {
	adAccountID := chi.URLParam(r, "id")
	var req updateAdAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.IsTracked == nil {
		writeError(w, http.StatusBadRequest, errors.New("is_tracked is required"))
		return
	}
	found, err := s.store.SetAdAccountTracked(r.Context(), adAccountID, *req.IsTracked)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, errors.New("ad account not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated", "is_tracked": *req.IsTracked})
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("invalid limit"))
			return
		}
		limit = parsed
	}
	alerts, err := s.store.ListAlerts(r.Context(), limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": alerts})
}

func parseTimeRange(r *http.Request) (time.Time, time.Time, error) {
	from, err := parseTimeQuery(r, "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseTimeQuery(r, "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	return from, to, nil
}

func parseTimeQuery(r *http.Request, key string) (time.Time, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return time.Time{}, fmt.Errorf("%s is required", key)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339", key)
	}
	return t.UTC(), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// internalError logs the real cause and returns a generic message so internal
// details (SQL, infrastructure) never reach API clients.
func (s *Server) internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}
