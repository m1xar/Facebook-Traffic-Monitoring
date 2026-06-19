package repository

import (
	"context"
	"fmt"
	"time"
)

type ActivityFilter string

const (
	ActivityFilterAll      ActivityFilter = "all"
	ActivityFilterActive   ActivityFilter = "active"
	ActivityFilterInactive ActivityFilter = "inactive"
)

func ParseActivityFilter(raw string) (ActivityFilter, error) {
	switch raw {
	case "", string(ActivityFilterAll):
		return ActivityFilterAll, nil
	case string(ActivityFilterActive):
		return ActivityFilterActive, nil
	case string(ActivityFilterInactive):
		return ActivityFilterInactive, nil
	default:
		return "", fmt.Errorf("activity_filter must be all, active, or inactive")
	}
}

func ValidActivityStatus(status string) bool {
	return status == string(ActivityFilterActive) || status == string(ActivityFilterInactive)
}

func AppendActivityFilter(conditions []string, args *[]any, alias string, filter ActivityFilter) []string {
	if filter == ActivityFilterAll || filter == "" {
		return conditions
	}
	*args = append(*args, string(filter))
	return append(conditions, fmt.Sprintf("%s.activity_status = $%d", alias, len(*args)))
}

func (s *Store) SetAdAccountActivityStatus(ctx context.Context, ids []string, status string) (int64, []string, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	if !ValidActivityStatus(status) {
		return 0, nil, fmt.Errorf("activity_status must be active or inactive")
	}
	rows, err := s.db.Query(ctx, `
		UPDATE ad_accounts
		SET activity_status = $2, updated_at = now()
		WHERE id = ANY($1)
		RETURNING id
	`, ids, status)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	updatedIDs := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, err
		}
		updatedIDs[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	missing := []string{}
	for _, id := range ids {
		if _, ok := updatedIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	return int64(len(updatedIDs)), missing, nil
}

func (s *Store) DeleteFBProfile(ctx context.Context, id int64) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM fb_profiles WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) MarkAccountResyncSuccess(ctx context.Context, profileID int64, at time.Time) error {
	resyncDate := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
	_, err := s.db.Exec(ctx, `
		UPDATE fb_profiles
		SET last_account_resync_at = $2,
		    last_account_resync_date = $3,
		    last_account_resync_error = NULL,
		    updated_at = now()
		WHERE id = $1
	`, profileID, at.UTC(), resyncDate)
	return err
}

func (s *Store) MarkAccountResyncFailure(ctx context.Context, profileID int64, errMessage string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fb_profiles
		SET last_account_resync_error = $2,
		    updated_at = now()
		WHERE id = $1
	`, profileID, errMessage)
	return err
}

type syncScheduleAccount struct {
	ProfileID int64
	AccountID string
	Index     int
	Total     int
	Offset    int
	UpdatedAt *time.Time
}

func (s *Store) NextUpdateTimes(ctx context.Context, accountIDs []string, batchSize int, cadence time.Duration, now time.Time) (map[string]*time.Time, error) {
	out := map[string]*time.Time{}
	if len(accountIDs) == 0 {
		return out, nil
	}
	if batchSize < 1 {
		batchSize = 1
	}
	rows, err := s.db.Query(ctx, `
		WITH sync_accounts AS (
			SELECT fpa.fb_profile_id,
			       a.id AS ad_account_id,
			       row_number() OVER (PARTITION BY fpa.fb_profile_id ORDER BY a.id) - 1 AS account_index,
			       count(*) OVER (PARTITION BY fpa.fb_profile_id) AS account_total
			FROM ad_accounts a
				JOIN fb_profile_ad_accounts fpa ON fpa.ad_account_id = a.id
				JOIN fb_profiles fp ON fp.id = fpa.fb_profile_id
				WHERE fpa.is_primary_for_sync
				  AND fpa.access_status = 'active'
				  AND a.is_tracked
		)
		SELECT sa.fb_profile_id,
		       sa.ad_account_id,
		       sa.account_index,
		       sa.account_total,
		       COALESCE(c.next_offset, 0) AS next_offset,
		       c.updated_at
		FROM sync_accounts sa
		LEFT JOIN sync_profile_cursors c ON c.fb_profile_id = sa.fb_profile_id
		WHERE sa.ad_account_id = ANY($1)
	`, accountIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	baseNow := now.UTC()
	for rows.Next() {
		var item syncScheduleAccount
		if err := rows.Scan(&item.ProfileID, &item.AccountID, &item.Index, &item.Total, &item.Offset, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if item.Total == 0 {
			continue
		}
		if item.Offset >= item.Total {
			item.Offset = 0
		}
		nextTick := baseNow
		if item.UpdatedAt != nil {
			candidate := item.UpdatedAt.UTC().Add(cadence)
			if candidate.After(nextTick) {
				nextTick = candidate
			}
		}
		batchesAway := batchesUntilIndex(item.Index, item.Offset, item.Total, batchSize)
		t := nextTick.Add(time.Duration(batchesAway) * cadence)
		out[item.AccountID] = &t
	}
	return out, rows.Err()
}

func batchesUntilIndex(index, offset, total, batchSize int) int {
	if total <= 0 || batchSize <= 0 {
		return 0
	}
	if offset >= total {
		offset = 0
	}
	if index >= offset {
		return (index - offset) / batchSize
	}
	remainingBatches := (total - offset + batchSize - 1) / batchSize
	return remainingBatches + index/batchSize
}
