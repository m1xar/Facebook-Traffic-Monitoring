package apihttp

import (
	"testing"
	"time"

	"meta-tracking/internal/domain"
)

func TestCalculateDeltasUsesLookbackSnapshot(t *testing.T) {
	from := time.Date(2026, 6, 17, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	snapshots := []domain.StatSnapshot{
		{
			AdAccountID: "act_1",
			CapturedAt:  from.Add(-10 * time.Minute),
			MetaDate:    dateOnly(2026, 6, 17),
			Metrics:     domain.SnapshotMetrics{Spend: 100, Impressions: 1000, Clicks: 50},
			Actions:     []domain.SnapshotAction{{ActionType: "lead", Count: 4, Value: 0}},
		},
		{
			AdAccountID: "act_1",
			CapturedAt:  from.Add(20 * time.Minute),
			MetaDate:    dateOnly(2026, 6, 17),
			Metrics:     domain.SnapshotMetrics{Spend: 145.5, Impressions: 1500, Clicks: 75},
			Actions:     []domain.SnapshotAction{{ActionType: "lead", Count: 9, Value: 0}},
		},
	}

	deltas := calculateDeltas(snapshots, from, to)
	if len(deltas) != 1 {
		t.Fatalf("expected one in-range delta, got %d", len(deltas))
	}
	got := deltas[0]
	if got.Metrics.Spend != 45.5 || got.Metrics.Impressions != 500 || got.Metrics.Clicks != 25 {
		t.Fatalf("unexpected metric delta: %+v", got.Metrics)
	}
	if got.Actions["lead"].Count != 5 {
		t.Fatalf("unexpected lead delta: %+v", got.Actions["lead"])
	}
}

func TestCalculateDeltasResetsOnNewMetaDate(t *testing.T) {
	from := time.Date(2026, 6, 17, 23, 50, 0, 0, time.UTC)
	to := from.Add(30 * time.Minute)
	snapshots := []domain.StatSnapshot{
		{
			AdAccountID: "act_1",
			CapturedAt:  from.Add(-5 * time.Minute),
			MetaDate:    dateOnly(2026, 6, 17),
			Metrics:     domain.SnapshotMetrics{Spend: 200, Impressions: 1000, Clicks: 50},
			Actions:     []domain.SnapshotAction{{ActionType: "purchase", Count: 3, Value: 300}},
		},
		{
			AdAccountID: "act_1",
			CapturedAt:  from.Add(20 * time.Minute),
			MetaDate:    dateOnly(2026, 6, 18),
			Metrics:     domain.SnapshotMetrics{Spend: 12, Impressions: 100, Clicks: 6},
			Actions:     []domain.SnapshotAction{{ActionType: "purchase", Count: 1, Value: 120}},
		},
	}

	deltas := calculateDeltas(snapshots, from, to)
	if len(deltas) != 1 {
		t.Fatalf("expected one in-range delta, got %d", len(deltas))
	}
	got := deltas[0]
	if got.Metrics.Spend != 12 || got.Metrics.Impressions != 100 || got.Metrics.Clicks != 6 {
		t.Fatalf("new meta_date must use absolute cumulative values: %+v", got.Metrics)
	}
	if got.Actions["purchase"].Count != 1 || got.Actions["purchase"].Value != 120 {
		t.Fatalf("new meta_date must use absolute action values: %+v", got.Actions["purchase"])
	}
}

func dateOnly(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
