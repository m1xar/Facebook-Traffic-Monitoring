package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"meta-tracking/internal/domain"
)

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type AdAccount struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AccountStatus *int    `json:"account_status,omitempty"`
	Currency      *string `json:"currency,omitempty"`
	TimezoneName  *string `json:"timezone_name,omitempty"`
}

// SyncAccount identifies an ad account to fetch insights for, along with the
// account timezone needed to anchor date_preset=today to the right meta_date.
type SyncAccount struct {
	ID       string
	Timezone string
}

type Client struct {
	baseURL     string
	version     string
	httpClient  *http.Client
	appID       string
	appSecret   string
	redirectURI string
	scopes      string
}

type Error struct {
	Code       int
	Subcode    int
	Type       string
	Message    string
	StatusCode int
}

func (e *Error) Error() string {
	return fmt.Sprintf("meta error code=%d subcode=%d: %s", e.Code, e.Subcode, e.Message)
}

// IsTokenError reports whether the error means the access token is no longer
// usable (expired, revoked, password change, checkpoint, etc.).
func (e *Error) IsTokenError() bool {
	return e.Code == 190 || e.Code == 102
}

// IsCheckpoint reports whether the token failure is a security checkpoint or
// unconfirmed-user state (the account itself needs operator attention).
func (e *Error) IsCheckpoint() bool {
	return e.IsTokenError() && (e.Subcode == 459 || e.Subcode == 464 || e.Subcode == 490)
}

// IsRateLimit reports whether the request was throttled and should be retried
// later without touching profile status.
func (e *Error) IsRateLimit() bool {
	switch e.Code {
	case 4, 17, 32, 613, 80000, 80003, 80004:
		return true
	}
	return false
}

// IsTransient reports a temporary Meta-side failure (codes 1 and 2: "an
// unexpected error has occurred, please retry") that should be retried
// without alerting or touching profile status.
func (e *Error) IsTransient() bool {
	return e.Code == 1 || e.Code == 2
}

func NewClient(version string) *Client {
	if version == "" {
		version = "v25.0"
	}
	return &Client{
		baseURL: "https://graph.facebook.com",
		version: version,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WithOAuth attaches the Meta app credentials needed for the server-side OAuth
// flow (authorization-code exchange and short-to-long-lived token exchange).
func (c *Client) WithOAuth(appID, appSecret, redirectURI, scopes string) *Client {
	c.appID = appID
	c.appSecret = appSecret
	c.redirectURI = redirectURI
	c.scopes = scopes
	return c
}

// AuthCodeURL builds the Facebook Login dialog URL the admin is redirected to.
// state is an opaque, signed value the callback uses to recover who started
// the flow.
func (c *Client) AuthCodeURL(state string) string {
	params := url.Values{}
	params.Set("client_id", c.appID)
	params.Set("redirect_uri", c.redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", c.scopes)
	params.Set("state", state)
	return fmt.Sprintf("https://www.facebook.com/%s/dialog/oauth?%s", c.version, params.Encode())
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// ExchangeCode trades the authorization code from the OAuth callback for a
// short-lived user access token.
func (c *Client) ExchangeCode(ctx context.Context, code string) (string, int64, error) {
	params := url.Values{}
	params.Set("client_id", c.appID)
	params.Set("client_secret", c.appSecret)
	params.Set("redirect_uri", c.redirectURI)
	params.Set("code", code)
	return c.tokenExchange(ctx, params)
}

// ExchangeLongLivedToken trades a short-lived token for a long-lived one
// (~60 days). This is the only way to obtain a durable token, and it requires
// the app secret, so it cannot be done client-side.
func (c *Client) ExchangeLongLivedToken(ctx context.Context, shortToken string) (string, int64, error) {
	params := url.Values{}
	params.Set("grant_type", "fb_exchange_token")
	params.Set("client_id", c.appID)
	params.Set("client_secret", c.appSecret)
	params.Set("fb_exchange_token", shortToken)
	return c.tokenExchange(ctx, params)
}

func (c *Client) tokenExchange(ctx context.Context, params url.Values) (string, int64, error) {
	path := fmt.Sprintf("/%s/oauth/access_token?%s", c.version, params.Encode())
	var out tokenResponse
	if err := c.do(ctx, http.MethodGet, path, "", "", nil, &out); err != nil {
		return "", 0, err
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("meta token exchange returned an empty access_token")
	}
	return out.AccessToken, out.ExpiresIn, nil
}

func (c *Client) Me(ctx context.Context, token string) (User, error) {
	var out User
	path := fmt.Sprintf("/%s/me?fields=id,name", c.version)
	if err := c.do(ctx, http.MethodGet, path, "", token, nil, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

func (c *Client) AdAccounts(ctx context.Context, token string) ([]AdAccount, error) {
	path := fmt.Sprintf("/%s/me/adaccounts?fields=id,name,account_status,currency,timezone_name&limit=500", c.version)
	var all []AdAccount
	for path != "" {
		var page struct {
			Data []struct {
				ID            string  `json:"id"`
				Name          string  `json:"name"`
				AccountStatus *int    `json:"account_status"`
				Currency      *string `json:"currency"`
				TimezoneName  *string `json:"timezone_name"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.do(ctx, http.MethodGet, path, "", token, nil, &page); err != nil {
			return nil, err
		}
		for _, account := range page.Data {
			all = append(all, AdAccount{
				ID:            account.ID,
				Name:          account.Name,
				AccountStatus: account.AccountStatus,
				Currency:      account.Currency,
				TimezoneName:  account.TimezoneName,
			})
		}
		if page.Paging.Next == "" {
			path = ""
		} else {
			nextURL, err := url.Parse(page.Paging.Next)
			if err != nil {
				return nil, err
			}
			path = nextURL.RequestURI()
		}
	}
	return all, nil
}

const insightsFields = "spend,impressions,clicks,reach,frequency,cpc,cpm,ctr,actions,action_values"

// InsightSnapshots fetches cumulative today-snapshots for the given accounts.
// Per-account failures that are not fatal for the whole profile (bad request,
// deleted account, etc.) are returned in the failed map; token and rate-limit
// errors abort the whole call.
func (c *Client) InsightSnapshots(ctx context.Context, token string, accounts []SyncAccount, capturedAt time.Time) (map[string]domain.StatSnapshot, map[string]error, error) {
	results := make(map[string]domain.StatSnapshot, len(accounts))
	failed := map[string]error{}
	for start := 0; start < len(accounts); start += 50 {
		end := start + 50
		if end > len(accounts) {
			end = len(accounts)
		}
		batch := make([]map[string]string, 0, end-start)
		for _, account := range accounts[start:end] {
			relativeURL := fmt.Sprintf("%s/insights?date_preset=today&fields=%s", account.ID, insightsFields)
			batch = append(batch, map[string]string{"method": http.MethodGet, "relative_url": relativeURL})
		}
		encodedBatch, err := json.Marshal(batch)
		if err != nil {
			return nil, nil, err
		}
		form := url.Values{}
		form.Set("access_token", token)
		form.Set("batch", string(encodedBatch))

		var batchOut []struct {
			Code int             `json:"code"`
			Body json.RawMessage `json:"body"`
		}
		if err := c.postBatch(ctx, form.Encode(), &batchOut); err != nil {
			return nil, nil, err
		}
		for i, item := range batchOut {
			account := accounts[start+i]
			body := decodeBatchBody(item.Body)
			if item.Code >= 400 {
				itemErr := parseBodyError(item.Code, body)
				var metaErr *Error
				if asMetaError(itemErr, &metaErr) && (metaErr.IsTokenError() || metaErr.IsRateLimit()) {
					return nil, nil, itemErr
				}
				failed[account.ID] = itemErr
				continue
			}
			metrics, actions, raw, err := parseInsightsBody(body)
			if err != nil {
				failed[account.ID] = fmt.Errorf("parse insights: %w", err)
				continue
			}
			results[account.ID] = domain.StatSnapshot{
				AdAccountID: account.ID,
				CapturedAt:  capturedAt.UTC(),
				MetaDate:    metaDate(capturedAt, account.Timezone),
				Metrics:     metrics,
				Actions:     actions,
				Raw:         raw,
			}
		}
	}
	return results, failed, nil
}

// postBatch sends one Graph batch request, retrying transient Meta-side
// failures (code 1/2) with a short backoff so a single blip does not abort
// the whole multi-batch sync.
func (c *Client) postBatch(ctx context.Context, encodedForm string, out any) error {
	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		err := c.do(ctx, http.MethodPost, "/"+c.version+"/", "application/x-www-form-urlencoded", "", strings.NewReader(encodedForm), out)
		if err == nil {
			return nil
		}
		var metaErr *Error
		if !asMetaError(err, &metaErr) || !metaErr.IsTransient() {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// metaDate resolves the calendar date of "today" in the ad account's own
// timezone, since date_preset=today resets at the account's midnight.
func metaDate(capturedAt time.Time, timezone string) time.Time {
	loc := time.UTC
	if timezone != "" {
		if parsed, err := time.LoadLocation(timezone); err == nil {
			loc = parsed
		}
	}
	local := capturedAt.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func asMetaError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// decodeBatchBody unwraps a Graph batch item body, which the API returns as a
// JSON-encoded string rather than a nested object.
func decodeBatchBody(raw json.RawMessage) []byte {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	return raw
}

func parseInsightsBody(body json.RawMessage) (domain.SnapshotMetrics, []domain.SnapshotAction, map[string]any, error) {
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return domain.SnapshotMetrics{}, nil, nil, err
	}
	raw := map[string]any{}
	_ = json.Unmarshal(body, &raw)
	if len(payload.Data) == 0 {
		return domain.SnapshotMetrics{}, nil, raw, nil
	}
	row := payload.Data[0]
	metrics := domain.SnapshotMetrics{
		Spend:       number(row["spend"]),
		Impressions: int64(number(row["impressions"])),
		Clicks:      int64(number(row["clicks"])),
		Reach:       int64(number(row["reach"])),
		Frequency:   number(row["frequency"]),
		CPC:         number(row["cpc"]),
		CPM:         number(row["cpm"]),
		CTR:         number(row["ctr"]),
	}
	return metrics, mergeActions(row["actions"], row["action_values"]), raw, nil
}

// mergeActions joins the actions (counts) and action_values (money) lists by
// action_type into one row per conversion type.
func mergeActions(counts, values any) []domain.SnapshotAction {
	merged := map[string]*domain.SnapshotAction{}
	collect := func(input any, apply func(a *domain.SnapshotAction, v float64)) {
		items, ok := input.([]any)
		if !ok {
			return
		}
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			actionType, _ := obj["action_type"].(string)
			if actionType == "" {
				continue
			}
			action := merged[actionType]
			if action == nil {
				action = &domain.SnapshotAction{ActionType: actionType}
				merged[actionType] = action
			}
			apply(action, number(obj["value"]))
		}
	}
	collect(counts, func(a *domain.SnapshotAction, v float64) { a.Count += v })
	collect(values, func(a *domain.SnapshotAction, v float64) { a.Value += v })

	out := make([]domain.SnapshotAction, 0, len(merged))
	for _, action := range merged {
		out = append(out, *action)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ActionType < out[j].ActionType })
	return out
}

func number(input any) float64 {
	switch v := input.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	default:
		return 0
	}
}

func (c *Client) do(ctx context.Context, method, path, contentType, token string, body io.Reader, out any) error {
	endpoint := c.baseURL + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		endpoint = path
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// The token goes in the Authorization header, never in the URL, so it
	// cannot leak through server access logs.
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return parseBodyError(resp.StatusCode, respBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(respBody))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	return nil
}

func parseBodyError(statusCode int, body []byte) error {
	var payload struct {
		Error struct {
			Message      string `json:"message"`
			Type         string `json:"type"`
			Code         int    `json:"code"`
			ErrorSubcode int    `json:"error_subcode"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Error.Message == "" {
		payload.Error.Message = string(body)
	}
	return &Error{
		Code:       payload.Error.Code,
		Subcode:    payload.Error.ErrorSubcode,
		Type:       payload.Error.Type,
		Message:    payload.Error.Message,
		StatusCode: statusCode,
	}
}
