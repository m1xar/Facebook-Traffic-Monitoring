package domain

import (
	"time"
)

type Role string

const (
	RoleAdmin Role = "admin"
	RoleBuyer Role = "buyer"
)

type Buyer struct {
	ID               int64     `json:"id"`
	CRMExternalID    *string   `json:"crm_external_id,omitempty"`
	DisplayName      string    `json:"display_name"`
	Email            *string   `json:"email,omitempty"`
	TelegramUsername *string   `json:"telegram_username,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type AppUser struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	BuyerID      *int64    `json:"buyer_id,omitempty"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type FBProfile struct {
	ID                     int64      `json:"id"`
	FBUserID               string     `json:"fb_user_id"`
	FBName                 string     `json:"fb_name"`
	AccessTokenCiphertext  string     `json:"-"`
	Status                 string     `json:"status"`
	StatusMessage          *string    `json:"status_message,omitempty"`
	TokenExpiresAt         *time.Time `json:"token_expires_at,omitempty"`
	LastAccountResyncAt    *time.Time `json:"last_account_resync_at,omitempty"`
	LastAccountResyncDate  *time.Time `json:"last_account_resync_date,omitempty"`
	LastAccountResyncError *string    `json:"last_account_resync_error,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type AdAccount struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	AccountStatus  *int       `json:"account_status,omitempty"`
	Currency       *string    `json:"currency,omitempty"`
	TimezoneName   *string    `json:"timezone_name,omitempty"`
	IsTracked      bool       `json:"is_tracked"`
	ActivityStatus string     `json:"activity_status"`
	LastUpdateAt   *time.Time `json:"last_update_at,omitempty"`
	NextUpdateAt   *time.Time `json:"next_update_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// SnapshotMetrics is the cumulative "today so far" statistics of an ad account
// at the moment a snapshot is captured, as reported by the Meta insights API.
type SnapshotMetrics struct {
	Spend       float64 `json:"spend"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	Reach       int64   `json:"reach"`
	Frequency   float64 `json:"frequency"`
	CPC         float64 `json:"cpc"`
	CPM         float64 `json:"cpm"`
	CTR         float64 `json:"ctr"`
}

// SnapshotAction is one conversion action type with its cumulative count and
// monetary value (actions / action_values merged by action_type).
type SnapshotAction struct {
	ActionType string  `json:"action_type"`
	Count      float64 `json:"count"`
	Value      float64 `json:"value"`
}

type StatSnapshot struct {
	ID          int64            `json:"id"`
	AdAccountID string           `json:"ad_account_id"`
	CapturedAt  time.Time        `json:"captured_at"`
	MetaDate    time.Time        `json:"meta_date"`
	Metrics     SnapshotMetrics  `json:"metrics"`
	Actions     []SnapshotAction `json:"actions"`
	Raw         map[string]any   `json:"-"`
}
