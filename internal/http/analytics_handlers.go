package apihttp

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"meta-tracking/internal/auth"
	"meta-tracking/internal/domain"
	"meta-tracking/internal/repository"

	"github.com/go-chi/chi/v5"
)

const defaultFreshnessStaleAfter = 2 * time.Hour

type analyticsParams struct {
	From        time.Time
	To          time.Time
	Timezone    *time.Location
	Granularity string
	BuyerID     *int64
	AdAccountID *string
}

type analyticsKPIs struct {
	Spend          float64 `json:"spend"`
	Impressions    int64   `json:"impressions"`
	Clicks         int64   `json:"clicks"`
	Reach          int64   `json:"reach"`
	Leads          float64 `json:"leads"`
	Purchases      float64 `json:"purchases"`
	PurchaseValue  float64 `json:"purchase_value"`
	CPL            float64 `json:"cpl"`
	CPA            float64 `json:"cpa"`
	ROAS           float64 `json:"roas"`
	CTR            float64 `json:"ctr"`
	CPC            float64 `json:"cpc"`
	CPM            float64 `json:"cpm"`
	Frequency      float64 `json:"frequency"`
	SnapshotPoints int     `json:"snapshot_points"`
}

type actionTotals map[string]domain.SnapshotAction

type analyticsDelta struct {
	AdAccountID string
	CapturedAt  time.Time
	MetaDate    time.Time
	Metrics     domain.SnapshotMetrics
	Actions     actionTotals
}

type analyticsAccumulator struct {
	Metrics        domain.SnapshotMetrics
	Actions        actionTotals
	AccountIDs     map[string]struct{}
	LastSnapshotAt *time.Time
	SnapshotPoints int
}

func newAnalyticsAccumulator() analyticsAccumulator {
	return analyticsAccumulator{
		Actions:    actionTotals{},
		AccountIDs: map[string]struct{}{},
	}
}

func (a *analyticsAccumulator) add(delta analyticsDelta) {
	a.Metrics.Spend += delta.Metrics.Spend
	a.Metrics.Impressions += delta.Metrics.Impressions
	a.Metrics.Clicks += delta.Metrics.Clicks
	a.Metrics.Reach += delta.Metrics.Reach
	a.AccountIDs[delta.AdAccountID] = struct{}{}
	a.SnapshotPoints++
	if a.LastSnapshotAt == nil || delta.CapturedAt.After(*a.LastSnapshotAt) {
		t := delta.CapturedAt
		a.LastSnapshotAt = &t
	}
	for key, action := range delta.Actions {
		current := a.Actions[key]
		current.ActionType = key
		current.Count += action.Count
		current.Value += action.Value
		a.Actions[key] = current
	}
}

func (a analyticsAccumulator) kpis() analyticsKPIs {
	leads := preferredActionCount(a.Actions, "lead", "offsite_conversion.fb_pixel_lead", "onsite_web_lead")
	purchases := preferredActionCount(a.Actions, "purchase", "offsite_conversion.fb_pixel_purchase", "omni_purchase")
	purchaseValue := preferredActionValue(a.Actions, "purchase", "offsite_conversion.fb_pixel_purchase", "omni_purchase")
	return analyticsKPIs{
		Spend:          round(a.Metrics.Spend, 2),
		Impressions:    a.Metrics.Impressions,
		Clicks:         a.Metrics.Clicks,
		Reach:          a.Metrics.Reach,
		Leads:          round(leads, 2),
		Purchases:      round(purchases, 2),
		PurchaseValue:  round(purchaseValue, 2),
		CPL:            safeDiv(a.Metrics.Spend, leads),
		CPA:            safeDiv(a.Metrics.Spend, purchases),
		ROAS:           safeDiv(purchaseValue, a.Metrics.Spend),
		CTR:            safePercent(float64(a.Metrics.Clicks), float64(a.Metrics.Impressions)),
		CPC:            safeDiv(a.Metrics.Spend, float64(a.Metrics.Clicks)),
		CPM:            safeDiv(a.Metrics.Spend*1000, float64(a.Metrics.Impressions)),
		Frequency:      safeDiv(float64(a.Metrics.Impressions), float64(a.Metrics.Reach)),
		SnapshotPoints: a.SnapshotPoints,
	}
}

func (s *Server) analyticsSummary(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	acc := newAnalyticsAccumulator()
	for _, delta := range deltas {
		acc.add(delta)
	}
	accounts, err := s.store.ListAdAccounts(r.Context(), params.BuyerID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	tracked := 0
	for _, account := range accounts {
		if account.IsTracked {
			tracked++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":                  params.From,
		"to":                    params.To,
		"timezone":              params.Timezone.String(),
		"kpis":                  acc.kpis(),
		"active_accounts":       len(acc.AccountIDs),
		"visible_accounts":      len(accounts),
		"tracked_accounts":      tracked,
		"last_snapshot_at":      acc.LastSnapshotAt,
		"data_freshness_status": freshnessStatus(acc.LastSnapshotAt, defaultFreshnessStaleAfter),
	})
}

func (s *Server) analyticsTimeseries(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	buckets := map[time.Time]analyticsAccumulator{}
	for _, delta := range deltas {
		key := bucketStart(delta.CapturedAt, params.Timezone, params.Granularity)
		acc := buckets[key]
		if acc.Actions == nil {
			acc = newAnalyticsAccumulator()
		}
		acc.add(delta)
		buckets[key] = acc
	}
	keys := make([]time.Time, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })

	series := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		series = append(series, map[string]any{
			"bucket_start":     key,
			"kpis":             buckets[key].kpis(),
			"active_accounts":  len(buckets[key].AccountIDs),
			"last_snapshot_at": buckets[key].LastSnapshotAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from":        params.From,
		"to":          params.To,
		"timezone":    params.Timezone.String(),
		"granularity": params.Granularity,
		"series":      series,
	})
}

func (s *Server) analyticsAdAccounts(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	byAccount := map[string]analyticsAccumulator{}
	for _, delta := range deltas {
		acc := byAccount[delta.AdAccountID]
		if acc.Actions == nil {
			acc = newAnalyticsAccumulator()
		}
		acc.add(delta)
		byAccount[delta.AdAccountID] = acc
	}

	accounts, err := s.store.ListAdAccounts(r.Context(), params.BuyerID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		acc := byAccount[account.ID]
		if acc.Actions == nil {
			acc = newAnalyticsAccumulator()
		}
		rows = append(rows, map[string]any{
			"ad_account":       account,
			"kpis":             acc.kpis(),
			"last_snapshot_at": acc.LastSnapshotAt,
			"freshness_status": freshnessStatus(acc.LastSnapshotAt, defaultFreshnessStaleAfter),
		})
	}
	sortAdAccountRows(rows, r.URL.Query().Get("sort"))
	if limit := parsePositiveInt(r.URL.Query().Get("limit")); limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

func (s *Server) analyticsBuyers(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.FromContext(r.Context())
	if claims.Role != domain.RoleAdmin {
		writeError(w, http.StatusForbidden, errors.New("buyer analytics are admin only"))
		return
	}
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	buyers, err := s.store.ListBuyers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(buyers))
	for _, buyer := range buyers {
		buyerID := buyer.ID
		localParams := *params
		localParams.BuyerID = &buyerID
		deltas, err := s.analyticsDeltas(r, &localParams)
		if err != nil {
			s.internalError(w, err)
			return
		}
		acc := newAnalyticsAccumulator()
		for _, delta := range deltas {
			acc.add(delta)
		}
		rows = append(rows, map[string]any{
			"buyer":            buyer,
			"kpis":             acc.kpis(),
			"active_accounts":  len(acc.AccountIDs),
			"last_snapshot_at": acc.LastSnapshotAt,
			"freshness_status": freshnessStatus(acc.LastSnapshotAt, defaultFreshnessStaleAfter),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["buyer"].(domain.Buyer).ID < rows[j]["buyer"].(domain.Buyer).ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

func (s *Server) analyticsAdAccountDetail(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	adAccountID := chiURLParam(r, "id")
	params.AdAccountID = &adAccountID

	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	acc := newAnalyticsAccumulator()
	for _, delta := range deltas {
		acc.add(delta)
	}

	detail := map[string]any{
		"ad_account_id":    adAccountID,
		"from":             params.From,
		"to":               params.To,
		"timezone":         params.Timezone.String(),
		"kpis":             acc.kpis(),
		"actions":          actionBreakdown(acc.Actions, acc.Metrics.Spend),
		"last_snapshot_at": acc.LastSnapshotAt,
		"freshness_status": freshnessStatus(acc.LastSnapshotAt, defaultFreshnessStaleAfter),
		"timeseries":       buildTimeseries(deltas, params),
	}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleAdmin {
		assignments, err := s.store.AccountAssignments(r.Context(), adAccountID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		detail["assignments"] = assignments
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) analyticsActions(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	acc := newAnalyticsAccumulator()
	for _, delta := range deltas {
		acc.add(delta)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": actionBreakdown(acc.Actions, acc.Metrics.Spend)})
}

func (s *Server) analyticsCompare(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	compareFrom, err := parseTimeQuery(r, "compare_from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	compareTo, err := parseTimeQuery(r, "compare_to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !compareTo.After(compareFrom) {
		writeError(w, http.StatusBadRequest, errors.New("compare_to must be after compare_from"))
		return
	}

	current, err := s.analyticsAggregate(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	previousParams := *params
	previousParams.From = compareFrom
	previousParams.To = compareTo
	previous, err := s.analyticsAggregate(r, &previousParams)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":        current.kpis(),
		"previous":       previous.kpis(),
		"delta":          kpiDelta(current.kpis(), previous.kpis()),
		"current_range":  map[string]time.Time{"from": params.From, "to": params.To},
		"previous_range": map[string]time.Time{"from": compareFrom, "to": compareTo},
	})
}

func (s *Server) analyticsPacing(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParamsWithPeriod(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	budget, err := strconv.ParseFloat(r.URL.Query().Get("budget"), 64)
	if err != nil || budget <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("budget must be a positive number"))
		return
	}
	acc, err := s.analyticsAggregate(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	now := time.Now().In(params.Timezone)
	totalSeconds := params.To.Sub(params.From).Seconds()
	elapsedSeconds := now.Sub(params.From.In(params.Timezone)).Seconds()
	if elapsedSeconds < 0 {
		elapsedSeconds = 0
	}
	if elapsedSeconds > totalSeconds {
		elapsedSeconds = totalSeconds
	}
	expected := budget * elapsedSeconds / totalSeconds
	projected := 0.0
	if elapsedSeconds > 0 {
		projected = acc.Metrics.Spend / elapsedSeconds * totalSeconds
	}
	status := "on_track"
	if acc.Metrics.Spend > expected*1.1 {
		status = "over"
	} else if acc.Metrics.Spend < expected*0.9 {
		status = "under"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"budget":                 round(budget, 2),
		"spend_so_far":           round(acc.Metrics.Spend, 2),
		"expected_spend_by_now":  round(expected, 2),
		"pacing_percent":         safePercent(acc.Metrics.Spend, expected),
		"projected_period_spend": round(projected, 2),
		"remaining_budget":       round(budget-acc.Metrics.Spend, 2),
		"status":                 status,
		"from":                   params.From,
		"to":                     params.To,
		"timezone":               params.Timezone.String(),
	})
}

func (s *Server) analyticsFreshness(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	items, err := s.store.AccountFreshness(r.Context(), params.BuyerID, defaultFreshnessStaleAfter)
	if err != nil {
		s.internalError(w, err)
		return
	}
	payload := map[string]any{"accounts": items, "stale_after_seconds": int64(defaultFreshnessStaleAfter.Seconds())}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleAdmin {
		profiles, err := s.store.ListFBProfiles(r.Context())
		if err != nil {
			s.internalError(w, err)
			return
		}
		payload["profiles"] = profiles
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) analyticsIssues(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	freshness, err := s.store.AccountFreshness(r.Context(), params.BuyerID, defaultFreshnessStaleAfter)
	if err != nil {
		s.internalError(w, err)
		return
	}
	issues := []map[string]any{}
	for _, item := range freshness {
		if item.FreshnessStatus == "fresh" || item.FreshnessStatus == "not_tracked" {
			continue
		}
		issues = append(issues, map[string]any{
			"type":          "DATA_FRESHNESS",
			"severity":      "warning",
			"ad_account_id": item.AdAccountID,
			"name":          item.Name,
			"status":        item.FreshnessStatus,
			"last_snapshot": item.LastSnapshotAt,
		})
	}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleAdmin {
		alerts, err := s.store.ListAlerts(r.Context(), 100)
		if err != nil {
			s.internalError(w, err)
			return
		}
		for _, alert := range alerts {
			issues = append(issues, map[string]any{
				"type":          alert.Type,
				"severity":      alertSeverity(alert.Type),
				"fb_profile_id": alert.FBProfileID,
				"ad_account_id": alert.AdAccountID,
				"buyer_id":      alert.BuyerID,
				"message":       alert.Message,
				"created_at":    alert.CreatedAt,
				"sent_at":       alert.SentAt,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": issues})
}

func (s *Server) analyticsExportCSV(w http.ResponseWriter, r *http.Request) {
	params, err := s.parseAnalyticsParams(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		s.internalError(w, err)
		return
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "ad_account"
	}
	rows := exportRows(deltas, params, groupBy)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="meta-analytics.csv"`)
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"group", "spend", "impressions", "clicks", "reach", "leads", "purchases", "purchase_value", "cpl", "cpa", "roas", "ctr", "cpc", "cpm"})
	for _, row := range rows {
		kpis := row.acc.kpis()
		_ = writer.Write([]string{
			row.group,
			formatFloat(kpis.Spend),
			strconv.FormatInt(kpis.Impressions, 10),
			strconv.FormatInt(kpis.Clicks, 10),
			strconv.FormatInt(kpis.Reach, 10),
			formatFloat(kpis.Leads),
			formatFloat(kpis.Purchases),
			formatFloat(kpis.PurchaseValue),
			formatFloat(kpis.CPL),
			formatFloat(kpis.CPA),
			formatFloat(kpis.ROAS),
			formatFloat(kpis.CTR),
			formatFloat(kpis.CPC),
			formatFloat(kpis.CPM),
		})
	}
	writer.Flush()
}

func (s *Server) analyticsAggregate(r *http.Request, params *analyticsParams) (analyticsAccumulator, error) {
	deltas, err := s.analyticsDeltas(r, params)
	if err != nil {
		return analyticsAccumulator{}, err
	}
	acc := newAnalyticsAccumulator()
	for _, delta := range deltas {
		acc.add(delta)
	}
	return acc, nil
}

func (s *Server) analyticsDeltas(r *http.Request, params *analyticsParams) ([]analyticsDelta, error) {
	snapshots, err := s.store.AnalyticsSnapshots(r.Context(), repository.AnalyticsSnapshotQuery{
		AdAccountID: params.AdAccountID,
		BuyerID:     params.BuyerID,
		From:        params.From,
		To:          params.To,
	})
	if err != nil {
		return nil, err
	}
	return calculateDeltas(snapshots, params.From, params.To), nil
}

func (s *Server) parseAnalyticsParams(r *http.Request, requireRange bool) (*analyticsParams, error) {
	loc := time.UTC
	if raw := r.URL.Query().Get("timezone"); raw != "" {
		parsed, err := time.LoadLocation(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid timezone")
		}
		loc = parsed
	}
	params := &analyticsParams{
		Timezone:    loc,
		Granularity: r.URL.Query().Get("granularity"),
	}
	if params.Granularity == "" {
		params.Granularity = "hour"
	}
	if params.Granularity != "hour" && params.Granularity != "day" {
		return nil, errors.New("granularity must be hour or day")
	}
	if requireRange || r.URL.Query().Get("from") != "" || r.URL.Query().Get("to") != "" {
		from, to, err := parseTimeRange(r)
		if err != nil {
			return nil, err
		}
		params.From = from
		params.To = to
	}
	if raw := r.URL.Query().Get("buyer_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, errors.New("invalid buyer_id")
		}
		params.BuyerID = &parsed
	}
	if raw := r.URL.Query().Get("ad_account_id"); raw != "" {
		params.AdAccountID = &raw
	}
	claims, _ := auth.FromContext(r.Context())
	if claims.Role == domain.RoleBuyer {
		params.BuyerID = claims.BuyerID
	}
	return params, nil
}

func (s *Server) parseAnalyticsParamsWithPeriod(r *http.Request) (*analyticsParams, error) {
	if r.URL.Query().Get("from") != "" || r.URL.Query().Get("to") != "" {
		return s.parseAnalyticsParams(r, true)
	}
	params, err := s.parseAnalyticsParams(r, false)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(params.Timezone)
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "today"
	}
	switch period {
	case "today":
		params.From = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, params.Timezone).UTC()
		params.To = params.From.In(params.Timezone).AddDate(0, 0, 1).UTC()
	case "week":
		dayOffset := (int(now.Weekday()) + 6) % 7
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, params.Timezone).AddDate(0, 0, -dayOffset)
		params.From = start.UTC()
		params.To = start.AddDate(0, 0, 7).UTC()
	case "month":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, params.Timezone)
		params.From = start.UTC()
		params.To = start.AddDate(0, 1, 0).UTC()
	default:
		return nil, errors.New("period must be today, week, or month")
	}
	return params, nil
}

func calculateDeltas(snapshots []domain.StatSnapshot, from, to time.Time) []analyticsDelta {
	previousByAccount := map[string]domain.StatSnapshot{}
	deltas := []analyticsDelta{}
	for _, snap := range snapshots {
		previous, hasPrevious := previousByAccount[snap.AdAccountID]
		if !snap.CapturedAt.Before(from) && snap.CapturedAt.Before(to) {
			deltas = append(deltas, snapshotDelta(snap, previous, hasPrevious))
		}
		previousByAccount[snap.AdAccountID] = snap
	}
	return deltas
}

func snapshotDelta(current, previous domain.StatSnapshot, hasPrevious bool) analyticsDelta {
	sameMetaDay := hasPrevious && current.MetaDate.Equal(previous.MetaDate)
	delta := analyticsDelta{
		AdAccountID: current.AdAccountID,
		CapturedAt:  current.CapturedAt,
		MetaDate:    current.MetaDate,
		Actions:     actionTotals{},
	}
	if sameMetaDay {
		delta.Metrics.Spend = clampFloat(current.Metrics.Spend - previous.Metrics.Spend)
		delta.Metrics.Impressions = clampInt(current.Metrics.Impressions - previous.Metrics.Impressions)
		delta.Metrics.Clicks = clampInt(current.Metrics.Clicks - previous.Metrics.Clicks)
		delta.Metrics.Reach = clampInt(current.Metrics.Reach - previous.Metrics.Reach)
	} else {
		delta.Metrics.Spend = current.Metrics.Spend
		delta.Metrics.Impressions = current.Metrics.Impressions
		delta.Metrics.Clicks = current.Metrics.Clicks
		delta.Metrics.Reach = current.Metrics.Reach
	}

	previousActions := map[string]domain.SnapshotAction{}
	if sameMetaDay {
		for _, action := range previous.Actions {
			previousActions[action.ActionType] = action
		}
	}
	for _, action := range current.Actions {
		prev := previousActions[action.ActionType]
		delta.Actions[action.ActionType] = domain.SnapshotAction{
			ActionType: action.ActionType,
			Count:      clampFloat(action.Count - prev.Count),
			Value:      clampFloat(action.Value - prev.Value),
		}
		if !sameMetaDay {
			delta.Actions[action.ActionType] = action
		}
	}
	return delta
}

func buildTimeseries(deltas []analyticsDelta, params *analyticsParams) []map[string]any {
	buckets := map[time.Time]analyticsAccumulator{}
	for _, delta := range deltas {
		key := bucketStart(delta.CapturedAt, params.Timezone, params.Granularity)
		acc := buckets[key]
		if acc.Actions == nil {
			acc = newAnalyticsAccumulator()
		}
		acc.add(delta)
		buckets[key] = acc
	}
	keys := make([]time.Time, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{"bucket_start": key, "kpis": buckets[key].kpis()})
	}
	return out
}

func actionBreakdown(actions actionTotals, spend float64) []map[string]any {
	totalCount := 0.0
	for _, action := range actions {
		totalCount += action.Count
	}
	keys := make([]string, 0, len(actions))
	for key := range actions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		action := actions[key]
		rows = append(rows, map[string]any{
			"action_type":      key,
			"count":            round(action.Count, 2),
			"value":            round(action.Value, 2),
			"share_of_actions": safePercent(action.Count, totalCount),
			"cost_per_action":  safeDiv(spend, action.Count),
			"value_per_action": safeDiv(action.Value, action.Count),
		})
	}
	return rows
}

func bucketStart(t time.Time, loc *time.Location, granularity string) time.Time {
	local := t.In(loc)
	if granularity == "day" {
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	}
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, loc)
}

func preferredActionCount(actions actionTotals, keys ...string) float64 {
	for _, key := range keys {
		if action, ok := actions[key]; ok && action.Count != 0 {
			return action.Count
		}
	}
	return 0
}

func preferredActionValue(actions actionTotals, keys ...string) float64 {
	for _, key := range keys {
		if action, ok := actions[key]; ok && action.Value != 0 {
			return action.Value
		}
	}
	return 0
}

func safeDiv(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return round(num/den, 4)
}

func safePercent(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return round(num/den*100, 4)
}

func round(v float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(v*pow) / pow
}

func clampFloat(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func clampInt(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func freshnessStatus(last *time.Time, staleAfter time.Duration) string {
	if last == nil {
		return "never_synced"
	}
	if time.Since(last.UTC()) > staleAfter {
		return "stale"
	}
	return "fresh"
}

func sortAdAccountRows(rows []map[string]any, sortKey string) {
	sort.Slice(rows, func(i, j int) bool {
		left := rows[i]["kpis"].(analyticsKPIs)
		right := rows[j]["kpis"].(analyticsKPIs)
		switch sortKey {
		case "roas_desc":
			return left.ROAS > right.ROAS
		case "cpl_asc":
			return left.CPL < right.CPL
		case "spend_desc", "":
			return left.Spend > right.Spend
		default:
			return left.Spend > right.Spend
		}
	})
}

func parsePositiveInt(raw string) int {
	if raw == "" {
		return 0
	}
	n, _ := strconv.Atoi(raw)
	if n < 0 {
		return 0
	}
	return n
}

func kpiDelta(current, previous analyticsKPIs) map[string]any {
	return map[string]any{
		"spend":          metricDelta(current.Spend, previous.Spend),
		"impressions":    metricDelta(float64(current.Impressions), float64(previous.Impressions)),
		"clicks":         metricDelta(float64(current.Clicks), float64(previous.Clicks)),
		"reach":          metricDelta(float64(current.Reach), float64(previous.Reach)),
		"leads":          metricDelta(current.Leads, previous.Leads),
		"purchases":      metricDelta(current.Purchases, previous.Purchases),
		"purchase_value": metricDelta(current.PurchaseValue, previous.PurchaseValue),
		"cpl":            metricDelta(current.CPL, previous.CPL),
		"cpa":            metricDelta(current.CPA, previous.CPA),
		"roas":           metricDelta(current.ROAS, previous.ROAS),
		"ctr":            metricDelta(current.CTR, previous.CTR),
		"cpc":            metricDelta(current.CPC, previous.CPC),
		"cpm":            metricDelta(current.CPM, previous.CPM),
	}
}

func metricDelta(current, previous float64) map[string]float64 {
	return map[string]float64{
		"absolute": round(current-previous, 4),
		"percent":  safePercent(current-previous, previous),
	}
}

func alertSeverity(alertType string) string {
	switch alertType {
	case "TOKEN_INVALID_OR_EXPIRED", "PROFILE_CHECKPOINT":
		return "critical"
	case "TOKEN_EXPIRING_SOON", "ACCOUNT_SYNC_FAILED":
		return "warning"
	default:
		return "info"
	}
}

type exportRow struct {
	group string
	acc   analyticsAccumulator
}

func exportRows(deltas []analyticsDelta, params *analyticsParams, groupBy string) []exportRow {
	grouped := map[string]analyticsAccumulator{}
	for _, delta := range deltas {
		group := delta.AdAccountID
		switch groupBy {
		case "hour":
			group = bucketStart(delta.CapturedAt, params.Timezone, "hour").Format(time.RFC3339)
		case "day":
			group = bucketStart(delta.CapturedAt, params.Timezone, "day").Format("2006-01-02")
		case "ad_account", "":
			group = delta.AdAccountID
		default:
			group = delta.AdAccountID
		}
		acc := grouped[group]
		if acc.Actions == nil {
			acc = newAnalyticsAccumulator()
		}
		acc.add(delta)
		grouped[group] = acc
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]exportRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, exportRow{group: key, acc: grouped[key]})
	}
	return rows
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
