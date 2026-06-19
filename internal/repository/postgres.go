package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"meta-tracking/internal/domain"
	"meta-tracking/internal/meta"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func (s *Store) Close() {
	s.db.Close()
}

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

type CreateAppUserParams struct {
	Email        string      `json:"email"`
	PasswordHash string      `json:"-"`
	Role         domain.Role `json:"role"`
	BuyerID      *int64      `json:"buyer_id"`
}

func (s *Store) CreateAppUser(ctx context.Context, p CreateAppUserParams) (domain.AppUser, error) {
	var user domain.AppUser
	err := s.db.QueryRow(ctx, `
		INSERT INTO app_users (email, password_hash, role, buyer_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, password_hash, role, buyer_id, status, created_at, updated_at
	`, normalizeEmail(p.Email), p.PasswordHash, p.Role, p.BuyerID).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.BuyerID,
		&user.Status, &user.CreatedAt, &user.UpdatedAt,
	)
	return user, err
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s *Store) AppUserByEmail(ctx context.Context, email string) (*domain.AppUser, error) {
	var user domain.AppUser
	err := s.db.QueryRow(ctx, `
		SELECT id, email, password_hash, role, buyer_id, status, created_at, updated_at
		FROM app_users
		WHERE email = $1
	`, normalizeEmail(email)).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.BuyerID,
		&user.Status, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &user, err
}

func (s *Store) AppUserByID(ctx context.Context, id int64) (*domain.AppUser, error) {
	var user domain.AppUser
	err := s.db.QueryRow(ctx, `
		SELECT id, email, password_hash, role, buyer_id, status, created_at, updated_at
		FROM app_users
		WHERE id = $1
	`, id).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.BuyerID,
		&user.Status, &user.CreatedAt, &user.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &user, err
}

func (s *Store) CountAppUsers(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM app_users`).Scan(&count)
	return count, err
}

func (s *Store) CreateRefreshToken(ctx context.Context, jti string, userID int64, expiresAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO refresh_tokens (jti, user_id, expires_at)
		VALUES ($1, $2, $3)
	`, jti, userID, expiresAt.UTC())
	return err
}

// ConsumeRefreshToken atomically revokes a live refresh token and returns its
// owner. Returns ok=false when the token is unknown, already used, or expired.
func (s *Store) ConsumeRefreshToken(ctx context.Context, jti string) (int64, bool, error) {
	var userID int64
	err := s.db.QueryRow(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE jti = $1 AND revoked_at IS NULL AND expires_at > now()
		RETURNING user_id
	`, jti).Scan(&userID)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return userID, true, nil
}

type CreateBuyerWithUserParams struct {
	DisplayName      string
	Email            string
	PasswordHash     string
	TelegramUsername *string
	CRMExternalID    *string
}

// CreateBuyerWithUser creates the buyer and its CRM login (app_users row with
// role buyer) in one transaction, so a buyer can never exist without
// credentials or vice versa.
func (s *Store) CreateBuyerWithUser(ctx context.Context, p CreateBuyerWithUserParams) (domain.Buyer, domain.AppUser, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Buyer{}, domain.AppUser{}, err
	}
	defer tx.Rollback(ctx)

	email := normalizeEmail(p.Email)
	var buyer domain.Buyer
	err = tx.QueryRow(ctx, `
		INSERT INTO buyers (crm_external_id, display_name, email, telegram_username)
		VALUES ($1, $2, $3, $4)
		RETURNING id, crm_external_id, display_name, email, telegram_username, created_at, updated_at
	`, p.CRMExternalID, p.DisplayName, email, p.TelegramUsername).Scan(
		&buyer.ID, &buyer.CRMExternalID, &buyer.DisplayName, &buyer.Email,
		&buyer.TelegramUsername, &buyer.CreatedAt, &buyer.UpdatedAt,
	)
	if err != nil {
		return domain.Buyer{}, domain.AppUser{}, err
	}

	var user domain.AppUser
	err = tx.QueryRow(ctx, `
		INSERT INTO app_users (email, password_hash, role, buyer_id)
		VALUES ($1, $2, 'buyer', $3)
		RETURNING id, email, password_hash, role, buyer_id, status, created_at, updated_at
	`, email, p.PasswordHash, buyer.ID).Scan(
		&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.BuyerID,
		&user.Status, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return domain.Buyer{}, domain.AppUser{}, err
	}
	return buyer, user, tx.Commit(ctx)
}

func (s *Store) UpsertFBProfile(ctx context.Context, fbUserID, fbName, tokenCiphertext string, tokenExpiresAt *time.Time) (domain.FBProfile, error) {
	var profile domain.FBProfile
	err := s.db.QueryRow(ctx, `
		INSERT INTO fb_profiles (fb_user_id, fb_name, access_token_ciphertext, token_expires_at, status, status_message, token_expiry_alert_sent_at)
		VALUES ($1, $2, $3, $4, 'active', NULL, NULL)
		ON CONFLICT (fb_user_id) DO UPDATE SET
			fb_name = EXCLUDED.fb_name,
			access_token_ciphertext = EXCLUDED.access_token_ciphertext,
			token_expires_at = EXCLUDED.token_expires_at,
			status = 'active',
			status_message = NULL,
			token_expiry_alert_sent_at = NULL,
			updated_at = now()
		RETURNING id, fb_user_id, fb_name, access_token_ciphertext, status, status_message, token_expires_at,
		          last_account_resync_at, last_account_resync_date, last_account_resync_error, created_at, updated_at
	`, fbUserID, fbName, tokenCiphertext, tokenExpiresAt).Scan(
		&profile.ID, &profile.FBUserID, &profile.FBName, &profile.AccessTokenCiphertext,
		&profile.Status, &profile.StatusMessage, &profile.TokenExpiresAt,
		&profile.LastAccountResyncAt, &profile.LastAccountResyncDate, &profile.LastAccountResyncError,
		&profile.CreatedAt, &profile.UpdatedAt,
	)
	return profile, err
}

func (s *Store) FBProfileByID(ctx context.Context, id int64) (*domain.FBProfile, error) {
	var p domain.FBProfile
	err := s.db.QueryRow(ctx, `
		SELECT id, fb_user_id, fb_name, access_token_ciphertext, status, status_message, token_expires_at,
		       last_account_resync_at, last_account_resync_date, last_account_resync_error, created_at, updated_at
		FROM fb_profiles WHERE id = $1
	`, id).Scan(
		&p.ID, &p.FBUserID, &p.FBName, &p.AccessTokenCiphertext,
		&p.Status, &p.StatusMessage, &p.TokenExpiresAt,
		&p.LastAccountResyncAt, &p.LastAccountResyncDate, &p.LastAccountResyncError,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// ExpiringProfile is an active profile whose long-lived token is approaching
// expiry and has not yet been alerted on.
type ExpiringProfile struct {
	ID             int64
	FBUserID       string
	FBName         string
	TokenExpiresAt time.Time
}

// ProfilesNeedingExpiryAlert returns active profiles whose token expires before
// cutoff and that have not been alerted since their last reconnect.
func (s *Store) ProfilesNeedingExpiryAlert(ctx context.Context, cutoff time.Time) ([]ExpiringProfile, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, fb_user_id, fb_name, token_expires_at
		FROM fb_profiles
		WHERE status = 'active'
		  AND token_expires_at IS NOT NULL
		  AND token_expires_at < $1
		  AND token_expiry_alert_sent_at IS NULL
		ORDER BY token_expires_at
	`, cutoff.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []ExpiringProfile
	for rows.Next() {
		var p ExpiringProfile
		if err := rows.Scan(&p.ID, &p.FBUserID, &p.FBName, &p.TokenExpiresAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// MarkExpiryAlerted stamps that an expiry alert has been sent so the scheduler
// does not re-alert on every tick until the profile is reconnected.
func (s *Store) MarkExpiryAlerted(ctx context.Context, profileID int64) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fb_profiles SET token_expiry_alert_sent_at = now() WHERE id = $1
	`, profileID)
	return err
}

// UpsertAdAccountsForProfile adds the profile's ad accounts to the tracking
// pool. Accounts already known from another profile are not duplicated: the
// row is updated and the existing primary-for-sync link is kept, so each
// account is fetched by exactly one profile.
func (s *Store) UpsertAdAccountsForProfile(ctx context.Context, profileID int64, accounts []meta.AdAccount) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, account := range accounts {
		if account.ID == "" {
			continue
		}
		name := account.Name
		if name == "" {
			name = account.ID
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO ad_accounts (id, name, account_status, currency, timezone_name)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name,
				account_status = EXCLUDED.account_status,
				currency = EXCLUDED.currency,
				timezone_name = EXCLUDED.timezone_name,
				updated_at = now()
		`, account.ID, name, account.AccountStatus, account.Currency, account.TimezoneName)
		if err != nil {
			return err
		}

		var hasPrimary bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM fb_profile_ad_accounts
				WHERE ad_account_id = $1 AND is_primary_for_sync
			)
		`, account.ID).Scan(&hasPrimary); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO fb_profile_ad_accounts (fb_profile_id, ad_account_id, is_primary_for_sync, access_status, last_seen_at)
			VALUES ($1, $2, $3, 'active', now())
			ON CONFLICT (fb_profile_id, ad_account_id) DO UPDATE SET
				access_status = 'active',
				last_seen_at = now()
		`, profileID, account.ID, !hasPrimary)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) AssignBuyer(ctx context.Context, adAccountID string, buyerID int64, assignedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := closeCurrentAssignment(ctx, tx, adAccountID, assignedAt); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO buyer_account_history (ad_account_id, buyer_id, assigned_at)
		VALUES ($1, $2, $3)
	`, adAccountID, buyerID, assignedAt.UTC())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func closeCurrentAssignment(ctx context.Context, tx pgx.Tx, adAccountID string, unassignedAt time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE buyer_account_history
		SET unassigned_at = $2
		WHERE ad_account_id = $1 AND unassigned_at IS NULL
	`, adAccountID, unassignedAt.UTC())
	return err
}

func (s *Store) UnassignBuyer(ctx context.Context, adAccountID string, unassignedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := closeCurrentAssignment(ctx, tx, adAccountID, unassignedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Assignment is one interval of buyer ownership over an ad account.
type Assignment struct {
	ID           int64      `json:"id"`
	AdAccountID  string     `json:"ad_account_id"`
	BuyerID      int64      `json:"buyer_id"`
	BuyerName    string     `json:"buyer_name"`
	AssignedAt   time.Time  `json:"assigned_at"`
	UnassignedAt *time.Time `json:"unassigned_at,omitempty"`
}

func (s *Store) AccountAssignments(ctx context.Context, adAccountID string) ([]Assignment, error) {
	rows, err := s.db.Query(ctx, `
		SELECT h.id, h.ad_account_id, h.buyer_id, b.display_name, h.assigned_at, h.unassigned_at
		FROM buyer_account_history h
		JOIN buyers b ON b.id = h.buyer_id
		WHERE h.ad_account_id = $1
		ORDER BY h.assigned_at
	`, adAccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assignments := []Assignment{}
	for rows.Next() {
		var a Assignment
		if err := rows.Scan(&a.ID, &a.AdAccountID, &a.BuyerID, &a.BuyerName, &a.AssignedAt, &a.UnassignedAt); err != nil {
			return nil, err
		}
		assignments = append(assignments, a)
	}
	return assignments, rows.Err()
}

type SyncProfile struct {
	ID                    int64
	FBUserID              string
	FBName                string
	AccessTokenCiphertext string
	LastAccountResyncDate *time.Time
}

func (s *Store) SyncProfiles(ctx context.Context) ([]SyncProfile, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, fb_user_id, fb_name, access_token_ciphertext, last_account_resync_date
		FROM fb_profiles
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []SyncProfile
	for rows.Next() {
		var profile SyncProfile
		if err := rows.Scan(&profile.ID, &profile.FBUserID, &profile.FBName, &profile.AccessTokenCiphertext, &profile.LastAccountResyncDate); err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (s *Store) SyncProfileByID(ctx context.Context, id int64) (*SyncProfile, error) {
	var profile SyncProfile
	err := s.db.QueryRow(ctx, `
		SELECT id, fb_user_id, fb_name, access_token_ciphertext, last_account_resync_date
		FROM fb_profiles
		WHERE id = $1
	`, id).Scan(&profile.ID, &profile.FBUserID, &profile.FBName, &profile.AccessTokenCiphertext, &profile.LastAccountResyncDate)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &profile, err
}

func (s *Store) TrackedPrimaryAccountsForProfile(ctx context.Context, profileID int64) ([]meta.SyncAccount, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.id, COALESCE(a.timezone_name, '')
		FROM ad_accounts a
		JOIN fb_profile_ad_accounts fpa ON fpa.ad_account_id = a.id
		WHERE fpa.fb_profile_id = $1
		  AND fpa.is_primary_for_sync
		  AND fpa.access_status = 'active'
		  AND a.is_tracked
		ORDER BY a.id
	`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []meta.SyncAccount
	for rows.Next() {
		var account meta.SyncAccount
		if err := rows.Scan(&account.ID, &account.Timezone); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// NextTrackedPrimaryAccountBatchForProfile returns the next ordered chunk of
// tracked accounts for a profile and advances a persistent round-robin cursor.
func (s *Store) NextTrackedPrimaryAccountBatchForProfile(ctx context.Context, profileID int64, limit int) ([]meta.SyncAccount, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT a.id, COALESCE(a.timezone_name, '')
		FROM ad_accounts a
		JOIN fb_profile_ad_accounts fpa ON fpa.ad_account_id = a.id
		WHERE fpa.fb_profile_id = $1
		  AND fpa.is_primary_for_sync
		  AND fpa.access_status = 'active'
		  AND a.is_tracked
		ORDER BY a.id
	`, profileID)
	if err != nil {
		return nil, err
	}
	var accounts []meta.SyncAccount
	for rows.Next() {
		var account meta.SyncAccount
		if err := rows.Scan(&account.ID, &account.Timezone); err != nil {
			rows.Close()
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(accounts) == 0 {
		return nil, tx.Commit(ctx)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO sync_profile_cursors (fb_profile_id, next_offset)
		VALUES ($1, 0)
		ON CONFLICT (fb_profile_id) DO NOTHING
	`, profileID)
	if err != nil {
		return nil, err
	}
	var offset int
	if err := tx.QueryRow(ctx, `
		SELECT next_offset
		FROM sync_profile_cursors
		WHERE fb_profile_id = $1
		FOR UPDATE
	`, profileID).Scan(&offset); err != nil {
		return nil, err
	}
	if offset >= len(accounts) {
		offset = 0
	}
	end := offset + limit
	if end > len(accounts) {
		end = len(accounts)
	}
	nextOffset := end
	if nextOffset >= len(accounts) {
		nextOffset = 0
	}
	batch := append([]meta.SyncAccount(nil), accounts[offset:end]...)
	_, err = tx.Exec(ctx, `
		UPDATE sync_profile_cursors
		SET next_offset = $2, updated_at = now()
		WHERE fb_profile_id = $1
	`, profileID, nextOffset)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
}

// SaveSnapshot upserts the cumulative snapshot row and replaces its action
// rows. Replacing (not merging) is correct because a snapshot is the full
// cumulative state at captured_at, so a retry simply rewrites the same facts.
func (s *Store) SaveSnapshot(ctx context.Context, snapshot domain.StatSnapshot) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	raw, _ := json.Marshal(snapshot.Raw)
	var snapshotID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO account_stat_snapshots
			(ad_account_id, captured_at, meta_date, spend, impressions, clicks, reach, frequency, cpc, cpm, ctr, raw)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (ad_account_id, captured_at) DO UPDATE SET
			meta_date = EXCLUDED.meta_date,
			spend = EXCLUDED.spend,
			impressions = EXCLUDED.impressions,
			clicks = EXCLUDED.clicks,
			reach = EXCLUDED.reach,
			frequency = EXCLUDED.frequency,
			cpc = EXCLUDED.cpc,
			cpm = EXCLUDED.cpm,
			ctr = EXCLUDED.ctr,
			raw = EXCLUDED.raw
		RETURNING id
	`, snapshot.AdAccountID, snapshot.CapturedAt.UTC(), snapshot.MetaDate,
		snapshot.Metrics.Spend, snapshot.Metrics.Impressions, snapshot.Metrics.Clicks,
		snapshot.Metrics.Reach, snapshot.Metrics.Frequency, snapshot.Metrics.CPC,
		snapshot.Metrics.CPM, snapshot.Metrics.CTR, raw).Scan(&snapshotID)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM snapshot_actions WHERE snapshot_id = $1`, snapshotID); err != nil {
		return err
	}
	for _, action := range snapshot.Actions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO snapshot_actions (snapshot_id, action_type, count, value)
			VALUES ($1, $2, $3, $4)
		`, snapshotID, action.ActionType, action.Count, action.Value); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// SnapshotQuery filters the snapshot listing. When BuyerID is set, only
// snapshots captured inside that buyer's ownership intervals
// [assigned_at, unassigned_at) are returned.
type SnapshotQuery struct {
	AdAccountID    *string
	BuyerID        *int64
	ActivityFilter ActivityFilter
	From           time.Time
	To             time.Time
}

func (s *Store) Snapshots(ctx context.Context, q SnapshotQuery) ([]domain.StatSnapshot, error) {
	conditions := []string{"s.captured_at >= $1", "s.captured_at < $2"}
	args := []any{q.From.UTC(), q.To.UTC()}
	if q.AdAccountID != nil {
		args = append(args, *q.AdAccountID)
		conditions = append(conditions, fmt.Sprintf("s.ad_account_id = $%d", len(args)))
	}
	if q.BuyerID != nil {
		args = append(args, *q.BuyerID)
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM buyer_account_history h
			WHERE h.ad_account_id = s.ad_account_id
			  AND h.buyer_id = $%d
			  AND s.captured_at >= h.assigned_at
			  AND (h.unassigned_at IS NULL OR s.captured_at < h.unassigned_at)
		)`, len(args)))
	}
	conditions = AppendActivityFilter(conditions, &args, "a", q.ActivityFilter)

	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT s.id, s.ad_account_id, s.captured_at, s.meta_date,
		       s.spend, s.impressions, s.clicks, s.reach, s.frequency, s.cpc, s.cpm, s.ctr
		FROM account_stat_snapshots s
		JOIN ad_accounts a ON a.id = s.ad_account_id
		WHERE %s
		ORDER BY s.ad_account_id, s.captured_at
	`, strings.Join(conditions, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := []domain.StatSnapshot{}
	ids := []int64{}
	for rows.Next() {
		var snap domain.StatSnapshot
		if err := rows.Scan(
			&snap.ID, &snap.AdAccountID, &snap.CapturedAt, &snap.MetaDate,
			&snap.Metrics.Spend, &snap.Metrics.Impressions, &snap.Metrics.Clicks, &snap.Metrics.Reach,
			&snap.Metrics.Frequency, &snap.Metrics.CPC, &snap.Metrics.CPM, &snap.Metrics.CTR,
		); err != nil {
			return nil, err
		}
		snap.Actions = []domain.SnapshotAction{}
		snapshots = append(snapshots, snap)
		ids = append(ids, snap.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return snapshots, nil
	}

	if err := s.attachActions(ctx, snapshots, ids); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (s *Store) attachActions(ctx context.Context, snapshots []domain.StatSnapshot, ids []int64) error {
	rows, err := s.db.Query(ctx, `
		SELECT snapshot_id, action_type, count, value
		FROM snapshot_actions
		WHERE snapshot_id = ANY($1)
		ORDER BY snapshot_id, action_type
	`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	byID := make(map[int64]*domain.StatSnapshot, len(snapshots))
	for i := range snapshots {
		byID[snapshots[i].ID] = &snapshots[i]
	}
	for rows.Next() {
		var snapshotID int64
		var action domain.SnapshotAction
		if err := rows.Scan(&snapshotID, &action.ActionType, &action.Count, &action.Value); err != nil {
			return err
		}
		if snap := byID[snapshotID]; snap != nil {
			snap.Actions = append(snap.Actions, action)
		}
	}
	return rows.Err()
}

func (s *Store) UpdateProfileStatus(ctx context.Context, profileID int64, status, message string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fb_profiles
		SET status = $2, status_message = $3, updated_at = now()
		WHERE id = $1
	`, profileID, status, message)
	return err
}

func (s *Store) CreateAlert(ctx context.Context, alertType string, profileID *int64, adAccountID *string, buyerID *int64, message string, payload map[string]any, sentAt *time.Time) error {
	payloadBytes, _ := json.Marshal(payload)
	_, err := s.db.Exec(ctx, `
		INSERT INTO alerts (type, fb_profile_id, ad_account_id, buyer_id, message, payload, sent_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, alertType, profileID, adAccountID, buyerID, message, payloadBytes, sentAt)
	return err
}

func (s *Store) ListBuyers(ctx context.Context) ([]domain.Buyer, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, crm_external_id, display_name, email, telegram_username, created_at, updated_at
		FROM buyers ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buyers := []domain.Buyer{}
	for rows.Next() {
		var b domain.Buyer
		if err := rows.Scan(&b.ID, &b.CRMExternalID, &b.DisplayName, &b.Email, &b.TelegramUsername, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		buyers = append(buyers, b)
	}
	return buyers, rows.Err()
}

func (s *Store) ListFBProfiles(ctx context.Context) ([]domain.FBProfile, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, fb_user_id, fb_name, access_token_ciphertext, status, status_message, token_expires_at,
		       last_account_resync_at, last_account_resync_date, last_account_resync_error, created_at, updated_at
		FROM fb_profiles ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	profiles := []domain.FBProfile{}
	for rows.Next() {
		var p domain.FBProfile
		if err := rows.Scan(
			&p.ID, &p.FBUserID, &p.FBName, &p.AccessTokenCiphertext, &p.Status, &p.StatusMessage, &p.TokenExpiresAt,
			&p.LastAccountResyncAt, &p.LastAccountResyncDate, &p.LastAccountResyncError, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

type AdAccountListItem struct {
	domain.AdAccount
	CurrentBuyerID   *int64  `json:"current_buyer_id,omitempty"`
	CurrentBuyerName *string `json:"current_buyer_name,omitempty"`
}

// ListAdAccounts returns all accounts with the currently assigned buyer.
// When buyerID is set, only accounts currently assigned to that buyer are returned.
func (s *Store) ListAdAccounts(ctx context.Context, buyerID *int64, activityFilter ActivityFilter) ([]AdAccountListItem, error) {
	args := []any{}
	conditions := []string{}
	if buyerID != nil {
		args = append(args, *buyerID)
		conditions = append(conditions, fmt.Sprintf("h.buyer_id = $%d", len(args)))
	}
	conditions = AppendActivityFilter(conditions, &args, "a", activityFilter)
	filter := ""
	if len(conditions) > 0 {
		filter = "WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT a.id, a.name, a.account_status, a.currency, a.timezone_name, a.is_tracked,
		       a.activity_status, max(s.captured_at), a.created_at, a.updated_at, h.buyer_id, b.display_name
		FROM ad_accounts a
		LEFT JOIN buyer_account_history h ON h.ad_account_id = a.id AND h.unassigned_at IS NULL
		LEFT JOIN buyers b ON b.id = h.buyer_id
		LEFT JOIN account_stat_snapshots s ON s.ad_account_id = a.id
		%s
		GROUP BY a.id, a.name, a.account_status, a.currency, a.timezone_name, a.is_tracked,
		         a.activity_status, a.created_at, a.updated_at, h.buyer_id, b.display_name
		ORDER BY a.id
	`, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := []AdAccountListItem{}
	for rows.Next() {
		var item AdAccountListItem
		if err := rows.Scan(
			&item.ID, &item.Name, &item.AccountStatus, &item.Currency, &item.TimezoneName,
			&item.IsTracked, &item.ActivityStatus, &item.LastUpdateAt, &item.CreatedAt, &item.UpdatedAt,
			&item.CurrentBuyerID, &item.CurrentBuyerName,
		); err != nil {
			return nil, err
		}
		accounts = append(accounts, item)
	}
	return accounts, rows.Err()
}

type Alert struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	FBProfileID *int64          `json:"fb_profile_id,omitempty"`
	AdAccountID *string         `json:"ad_account_id,omitempty"`
	BuyerID     *int64          `json:"buyer_id,omitempty"`
	Message     string          `json:"message"`
	Payload     json.RawMessage `json:"payload"`
	SentAt      *time.Time      `json:"sent_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

func (s *Store) ListAlerts(ctx context.Context, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, type, fb_profile_id, ad_account_id, buyer_id, message, payload, sent_at, created_at
		FROM alerts ORDER BY id DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	alerts := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Type, &a.FBProfileID, &a.AdAccountID, &a.BuyerID, &a.Message, &a.Payload, &a.SentAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}
