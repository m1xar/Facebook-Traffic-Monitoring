package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"meta-tracking/internal/domain"
)

// AnalyticsSnapshotQuery fetches snapshots for read-model calculations. The
// result includes the latest snapshot before From for every account in the
// range, so callers can turn cumulative Meta values into interval deltas.
type AnalyticsSnapshotQuery struct {
	AdAccountID *string
	BuyerID     *int64
	From        time.Time
	To          time.Time
}

func (s *Store) AnalyticsSnapshots(ctx context.Context, q AnalyticsSnapshotQuery) ([]domain.StatSnapshot, error) {
	conditions := []string{"s.captured_at >= $1", "s.captured_at < $2"}
	args := []any{q.From.UTC(), q.To.UTC()}
	if q.AdAccountID != nil {
		args = append(args, *q.AdAccountID)
		conditions = append(conditions, fmt.Sprintf("s.ad_account_id = $%d", len(args)))
	}
	if q.BuyerID != nil {
		args = append(args, *q.BuyerID)
		conditions = append(conditions, buyerSnapshotCondition("s", len(args)))
	}

	previousConditions := []string{"s.captured_at < $1"}
	if q.AdAccountID != nil {
		previousConditions = append(previousConditions, fmt.Sprintf("s.ad_account_id = $%d", indexOfAdAccountArg(q)))
	}
	if q.BuyerID != nil {
		previousConditions = append(previousConditions, buyerSnapshotCondition("s", len(args)))
	}

	query := fmt.Sprintf(`
		WITH base_accounts AS (
			SELECT DISTINCT s.ad_account_id
			FROM account_stat_snapshots s
			WHERE %s
		),
		in_range AS (
			SELECT s.id, s.ad_account_id, s.captured_at, s.meta_date,
			       s.spend, s.impressions, s.clicks, s.reach, s.frequency, s.cpc, s.cpm, s.ctr
			FROM account_stat_snapshots s
			WHERE %s
		),
		previous AS (
			SELECT DISTINCT ON (s.ad_account_id)
			       s.id, s.ad_account_id, s.captured_at, s.meta_date,
			       s.spend, s.impressions, s.clicks, s.reach, s.frequency, s.cpc, s.cpm, s.ctr
			FROM account_stat_snapshots s
			JOIN base_accounts ba ON ba.ad_account_id = s.ad_account_id
			WHERE %s
			ORDER BY s.ad_account_id, s.captured_at DESC
		)
		SELECT id, ad_account_id, captured_at, meta_date,
		       spend, impressions, clicks, reach, frequency, cpc, cpm, ctr
		FROM (
			SELECT * FROM previous
			UNION ALL
			SELECT * FROM in_range
		) x
		ORDER BY ad_account_id, captured_at
	`, strings.Join(conditions, " AND "), strings.Join(conditions, " AND "), strings.Join(previousConditions, " AND "))

	rows, err := s.db.Query(ctx, query, args...)
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

func buyerSnapshotCondition(alias string, argIndex int) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM buyer_account_history h
		WHERE h.ad_account_id = %[1]s.ad_account_id
		  AND h.buyer_id = $%[2]d
		  AND %[1]s.captured_at >= h.assigned_at
		  AND (h.unassigned_at IS NULL OR %[1]s.captured_at < h.unassigned_at)
	)`, alias, argIndex)
}

func indexOfAdAccountArg(q AnalyticsSnapshotQuery) int {
	if q.AdAccountID == nil {
		return 0
	}
	return 3
}

type AccountFreshness struct {
	AdAccountID        string     `json:"ad_account_id"`
	Name               string     `json:"name"`
	IsTracked          bool       `json:"is_tracked"`
	CurrentBuyerID     *int64     `json:"current_buyer_id,omitempty"`
	CurrentBuyerName   *string    `json:"current_buyer_name,omitempty"`
	LastSnapshotAt     *time.Time `json:"last_snapshot_at,omitempty"`
	LastSnapshotAgeSec *int64     `json:"last_snapshot_age_seconds,omitempty"`
	FreshnessStatus    string     `json:"freshness_status"`
}

func (s *Store) AccountFreshness(ctx context.Context, buyerID *int64, staleAfter time.Duration) ([]AccountFreshness, error) {
	args := []any{}
	filter := ""
	if buyerID != nil {
		args = append(args, *buyerID)
		filter = "WHERE h.buyer_id = $1"
	}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`
		SELECT a.id, a.name, a.is_tracked, h.buyer_id, b.display_name, max(s.captured_at)
		FROM ad_accounts a
		LEFT JOIN buyer_account_history h ON h.ad_account_id = a.id AND h.unassigned_at IS NULL
		LEFT JOIN buyers b ON b.id = h.buyer_id
		LEFT JOIN account_stat_snapshots s ON s.ad_account_id = a.id
		%s
		GROUP BY a.id, a.name, a.is_tracked, h.buyer_id, b.display_name
		ORDER BY a.id
	`, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	items := []AccountFreshness{}
	for rows.Next() {
		var item AccountFreshness
		if err := rows.Scan(&item.AdAccountID, &item.Name, &item.IsTracked, &item.CurrentBuyerID, &item.CurrentBuyerName, &item.LastSnapshotAt); err != nil {
			return nil, err
		}
		item.FreshnessStatus = "never_synced"
		if item.LastSnapshotAt != nil {
			age := int64(now.Sub(item.LastSnapshotAt.UTC()).Seconds())
			item.LastSnapshotAgeSec = &age
			if now.Sub(item.LastSnapshotAt.UTC()) > staleAfter {
				item.FreshnessStatus = "stale"
			} else {
				item.FreshnessStatus = "fresh"
			}
		}
		if !item.IsTracked {
			item.FreshnessStatus = "not_tracked"
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
