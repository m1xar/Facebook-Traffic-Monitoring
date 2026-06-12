package meta

import (
	"encoding/json"
	"net/url"
	"testing"

	"meta-tracking/internal/domain"
)

func TestParseInsightsBody(t *testing.T) {
	body := json.RawMessage(`{
		"data": [{
			"spend": "12.34",
			"impressions": "100",
			"clicks": "5",
			"reach": "80",
			"frequency": "1.25",
			"cpc": "2.468",
			"cpm": "123.4",
			"ctr": "5.0",
			"actions": [
				{"action_type": "lead", "value": "2"},
				{"action_type": "lead", "value": "3"},
				{"action_type": "purchase", "value": "1"}
			],
			"action_values": [
				{"action_type": "purchase", "value": "40.50"}
			]
		}]
	}`)

	metrics, actions, _, err := parseInsightsBody(body)
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if metrics.Spend != 12.34 || metrics.Impressions != 100 || metrics.Clicks != 5 {
		t.Fatalf("basic metrics mismatch: %+v", metrics)
	}
	if metrics.Reach != 80 || metrics.Frequency != 1.25 || metrics.CPC != 2.468 || metrics.CPM != 123.4 || metrics.CTR != 5.0 {
		t.Fatalf("extended metrics mismatch: %+v", metrics)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 merged action types, got %+v", actions)
	}
	// Actions are sorted by action_type: lead, purchase.
	if actions[0].ActionType != "lead" || actions[0].Count != 5 || actions[0].Value != 0 {
		t.Fatalf("lead action mismatch: %+v", actions[0])
	}
	if actions[1].ActionType != "purchase" || actions[1].Count != 1 || actions[1].Value != 40.50 {
		t.Fatalf("purchase action mismatch: %+v", actions[1])
	}
}

func TestParseInsightsBodyEmpty(t *testing.T) {
	metrics, actions, _, err := parseInsightsBody(json.RawMessage(`{"data":[]}`))
	if err != nil {
		t.Fatalf("parse empty body: %v", err)
	}
	if metrics != (domain.SnapshotMetrics{}) || len(actions) != 0 {
		t.Fatalf("empty insights must yield zero metrics: %+v %+v", metrics, actions)
	}
}

func TestDecodeBatchBody(t *testing.T) {
	// Graph batch items carry their body as a JSON-encoded string.
	encoded, err := json.Marshal(`{"data":[{"spend":"1.50"}]}`)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	metrics, _, _, parseErr := parseInsightsBody(decodeBatchBody(encoded))
	if parseErr != nil {
		t.Fatalf("parse decoded batch body: %v", parseErr)
	}
	if metrics.Spend != 1.50 {
		t.Fatalf("spend mismatch: %+v", metrics)
	}

	plain := json.RawMessage(`{"data":[]}`)
	if string(decodeBatchBody(plain)) != string(plain) {
		t.Fatalf("plain object body must pass through unchanged")
	}
}

func TestAuthCodeURL(t *testing.T) {
	client := NewClient("v25.0").WithOAuth("appid123", "secret", "https://api.example.com/oauth/facebook/callback", "ads_read,business_management")

	raw := client.AuthCodeURL("state-token")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("auth url did not parse: %v", err)
	}
	if parsed.Host != "www.facebook.com" || parsed.Path != "/v25.0/dialog/oauth" {
		t.Fatalf("unexpected dialog endpoint: %s", raw)
	}

	q := parsed.Query()
	cases := map[string]string{
		"client_id":     "appid123",
		"redirect_uri":  "https://api.example.com/oauth/facebook/callback",
		"response_type": "code",
		"scope":         "ads_read,business_management",
		"state":         "state-token",
	}
	for key, want := range cases {
		if got := q.Get(key); got != want {
			t.Fatalf("param %s = %q, want %q", key, got, want)
		}
	}
	// The app secret must never appear in the user-facing dialog URL.
	if q.Get("client_secret") != "" {
		t.Fatalf("client_secret leaked into auth url")
	}
}
