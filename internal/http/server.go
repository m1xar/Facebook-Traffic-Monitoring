package apihttp

import (
	"context"
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
	router        chi.Router
	store         *repository.Store
	metaClient    *meta.Client
	tokenCipher   *crypto.TokenCipher
	jwtSecret     string
	asynqClient   *asynq.Client
	syncInterval  time.Duration
	syncBatchSize int
}

// asynqClient may be nil; then the first sync waits for the scheduler.
func NewServer(store *repository.Store, metaClient *meta.Client, tokenCipher *crypto.TokenCipher, jwtSecret string, asynqClient *asynq.Client, syncInterval time.Duration, syncBatchSize int) *Server {
	if syncBatchSize < 1 {
		syncBatchSize = 1
	}
	s := &Server{
		router:        chi.NewRouter(),
		store:         store,
		metaClient:    metaClient,
		tokenCipher:   tokenCipher,
		jwtSecret:     jwtSecret,
		asynqClient:   asynqClient,
		syncInterval:  syncInterval,
		syncBatchSize: syncBatchSize,
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
		admin.Delete("/fb-profiles/{id}", s.deleteFBProfile)

		anyRole.Get("/ad-accounts", s.listAdAccounts)
		anyRole.Get("/ad-accounts/{id}/snapshots", s.accountSnapshots)
		admin.Get("/ad-accounts/{id}/assignments", s.accountAssignments)
		admin.Post("/ad-accounts/{id}/assign", s.assignBuyer)
		admin.Post("/ad-accounts/{id}/unassign", s.unassignBuyer)
		admin.Patch("/ad-accounts/activity-status", s.updateAdAccountActivityStatus)

		anyRole.Get("/snapshots", s.listSnapshots)

		admin.Get("/alerts", s.listAlerts)

		anyRole.Get("/analytics/summary", s.analyticsSummary)
		anyRole.Get("/analytics/timeseries", s.analyticsTimeseries)
		anyRole.Get("/analytics/ad-accounts", s.analyticsAdAccounts)
		anyRole.Get("/analytics/ad-accounts/{id}", s.analyticsAdAccountDetail)
		anyRole.Get("/analytics/actions", s.analyticsActions)
		anyRole.Get("/analytics/compare", s.analyticsCompare)
		anyRole.Get("/analytics/pacing", s.analyticsPacing)
		anyRole.Get("/analytics/freshness", s.analyticsFreshness)
		anyRole.Get("/analytics/issues", s.analyticsIssues)
		anyRole.Get("/analytics/export.csv", s.analyticsExportCSV)
		anyRole.Get("/analytics/buyers", s.analyticsBuyers)
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
	if err := s.store.MarkAccountResyncSuccess(r.Context(), profile.ID, time.Now().UTC()); err != nil {
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
	activityFilter, err := parseActivityFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	claims, _ := auth.FromContext(r.Context())
	var buyerID *int64
	if claims.Role == domain.RoleBuyer {
		buyerID = claims.BuyerID
	}
	accounts, err := s.store.ListAdAccounts(r.Context(), buyerID, activityFilter)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.attachNextUpdateToAdAccounts(r.Context(), accounts); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity_filter": activityFilter,
		"last_update_at":  maxLastUpdate(accounts),
		"next_update_at":  nearestNextUpdate(accounts),
		"data":            accounts,
	})
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

	activityFilter, err := parseActivityFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	query := repository.SnapshotQuery{AdAccountID: &adAccountID, From: from, To: to, ActivityFilter: activityFilter}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleBuyer {
		query.BuyerID = claims.BuyerID
	}

	snapshots, err := s.store.Snapshots(r.Context(), query)
	if err != nil {
		s.internalError(w, err)
		return
	}
	lastUpdateAt, nextUpdateAt, err := s.snapshotUpdateMetadata(r.Context(), snapshots)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity_filter": activityFilter,
		"last_update_at":  lastUpdateAt,
		"next_update_at":  nextUpdateAt,
		"data":            snapshots,
	})
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

	activityFilter, err := parseActivityFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	query := repository.SnapshotQuery{From: from, To: to, ActivityFilter: activityFilter}
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
	lastUpdateAt, nextUpdateAt, err := s.snapshotUpdateMetadata(r.Context(), snapshots)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity_filter": activityFilter,
		"last_update_at":  lastUpdateAt,
		"next_update_at":  nextUpdateAt,
		"data":            snapshots,
	})
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

func (s *Server) deleteFBProfile(w http.ResponseWriter, r *http.Request) {
	profileID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid profile id"))
		return
	}
	found, err := s.store.DeleteFBProfile(r.Context(), profileID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, errors.New("fb profile not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

type updateAdAccountActivityStatusRequest struct {
	AdAccountIDs   []string `json:"ad_account_ids"`
	ActivityStatus string   `json:"activity_status"`
}

func (s *Server) updateAdAccountActivityStatus(w http.ResponseWriter, r *http.Request) {
	var req updateAdAccountActivityStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.AdAccountIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("ad_account_ids is required"))
		return
	}
	if !repository.ValidActivityStatus(req.ActivityStatus) {
		writeError(w, http.StatusBadRequest, errors.New("activity_status must be active or inactive"))
		return
	}
	updated, missing, err := s.store.SetAdAccountActivityStatus(r.Context(), req.AdAccountIDs, req.ActivityStatus)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "missing_ids": missing})
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

func parseActivityFilter(r *http.Request) (repository.ActivityFilter, error) {
	return repository.ParseActivityFilter(r.URL.Query().Get("activity_filter"))
}

func (s *Server) attachNextUpdateToAdAccounts(ctx context.Context, accounts []repository.AdAccountListItem) error {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	nextUpdates, err := s.store.NextUpdateTimes(ctx, ids, s.syncBatchSize, s.syncInterval, time.Now().UTC())
	if err != nil {
		return err
	}
	for i := range accounts {
		accounts[i].NextUpdateAt = nextUpdates[accounts[i].ID]
	}
	return nil
}

func (s *Server) snapshotUpdateMetadata(ctx context.Context, snapshots []domain.StatSnapshot) (*time.Time, *time.Time, error) {
	var lastUpdateAt *time.Time
	seen := map[string]struct{}{}
	ids := []string{}
	for _, snapshot := range snapshots {
		if lastUpdateAt == nil || snapshot.CapturedAt.After(*lastUpdateAt) {
			t := snapshot.CapturedAt
			lastUpdateAt = &t
		}
		if _, ok := seen[snapshot.AdAccountID]; !ok {
			seen[snapshot.AdAccountID] = struct{}{}
			ids = append(ids, snapshot.AdAccountID)
		}
	}
	nextUpdates, err := s.store.NextUpdateTimes(ctx, ids, s.syncBatchSize, s.syncInterval, time.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	return lastUpdateAt, nearestTime(nextUpdates), nil
}

func nearestTime(times map[string]*time.Time) *time.Time {
	var nearest *time.Time
	for _, candidate := range times {
		if candidate == nil {
			continue
		}
		if nearest == nil || candidate.Before(*nearest) {
			t := *candidate
			nearest = &t
		}
	}
	return nearest
}

func nearestNextUpdate(accounts []repository.AdAccountListItem) *time.Time {
	var nearest *time.Time
	for _, account := range accounts {
		if account.NextUpdateAt == nil {
			continue
		}
		if nearest == nil || account.NextUpdateAt.Before(*nearest) {
			t := *account.NextUpdateAt
			nearest = &t
		}
	}
	return nearest
}

func maxLastUpdate(accounts []repository.AdAccountListItem) *time.Time {
	var latest *time.Time
	for _, account := range accounts {
		if account.LastUpdateAt == nil {
			continue
		}
		if latest == nil || account.LastUpdateAt.After(*latest) {
			t := *account.LastUpdateAt
			latest = &t
		}
	}
	return latest
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
