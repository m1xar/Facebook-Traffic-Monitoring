package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"meta-tracking/internal/crypto"
	"meta-tracking/internal/meta"
	"meta-tracking/internal/repository"
	"meta-tracking/internal/telegram"

	"github.com/hibiken/asynq"
)

const TypeSyncProfile = "meta:sync-profile"

type SyncProfilePayload struct {
	ProfileID int64 `json:"profile_id"`
}

type Processor struct {
	store       *repository.Store
	metaClient  *meta.Client
	tokenCipher *crypto.TokenCipher
	telegram    *telegram.Client
	batchSize   int
}

func NewProcessor(store *repository.Store, metaClient *meta.Client, tokenCipher *crypto.TokenCipher, telegramClient *telegram.Client, batchSize int) *Processor {
	if batchSize < 1 {
		batchSize = 1
	}
	return &Processor{
		store:       store,
		metaClient:  metaClient,
		tokenCipher: tokenCipher,
		telegram:    telegramClient,
		batchSize:   batchSize,
	}
}

func NewSyncProfileTask(profileID int64) (*asynq.Task, error) {
	payload, err := json.Marshal(SyncProfilePayload{ProfileID: profileID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeSyncProfile, payload), nil
}

func (p *Processor) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeSyncProfile, p.HandleSyncProfile)
}

// EnqueueSyncProfile enqueues one round-robin sync chunk for a profile.
// Uniqueness spans just under the scheduler cadence, so a slow tick cannot
// stack duplicate chunks for the same profile.
func EnqueueSyncProfile(ctx context.Context, client *asynq.Client, profileID int64, delay, cadence time.Duration) error {
	task, err := NewSyncProfileTask(profileID)
	if err != nil {
		return err
	}
	_, err = client.EnqueueContext(ctx, task,
		asynq.ProcessIn(delay), asynq.MaxRetry(5), asynq.Timeout(10*time.Minute), asynq.Unique(uniqueWindow(cadence)))
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

// uniqueWindow is the task-uniqueness TTL: just under the scheduler cadence
// (10m -> 9m10s), so the next intended tick is never blocked by its own lock.
func uniqueWindow(cadence time.Duration) time.Duration {
	if cadence <= time.Minute {
		return cadence
	}
	return cadence - cadence/12
}

func (p *Processor) HandleSyncProfile(ctx context.Context, task *asynq.Task) error {
	var payload SyncProfilePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("decode payload: %w", asynq.SkipRetry)
	}

	profile, err := p.store.ActiveProfileByID(ctx, payload.ProfileID)
	if err != nil {
		return err
	}
	if profile == nil {
		return nil
	}

	accounts, err := p.store.NextTrackedPrimaryAccountBatchForProfile(ctx, profile.ID, p.batchSize)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}

	token, err := p.tokenCipher.Decrypt(profile.AccessTokenCiphertext)
	if err != nil {
		return err
	}

	capturedAt := time.Now().UTC()
	log.Printf("sync profile %d: syncing %d account(s)", profile.ID, len(accounts))
	snapshots, failed, err := p.metaClient.InsightSnapshots(ctx, token, accounts, capturedAt)
	if err != nil {
		return p.handleSyncError(ctx, profile, err)
	}
	for accountID, accountErr := range failed {
		log.Printf("sync profile %d: account %s failed: %v", profile.ID, accountID, accountErr)
		id := accountID
		if createErr := p.store.CreateAlert(ctx, "ACCOUNT_SYNC_FAILED", &profile.ID, &id, nil, accountErr.Error(), map[string]any{
			"fb_user_id": profile.FBUserID,
			"fb_name":    profile.FBName,
		}, nil); createErr != nil {
			log.Printf("create alert: %v", createErr)
		}
	}
	for _, snapshot := range snapshots {
		if err := p.store.SaveSnapshot(ctx, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) handleSyncError(ctx context.Context, profile *repository.SyncProfile, err error) error {
	alertType := "UNKNOWN_ERROR"
	status := ""
	var metaErr *meta.Error
	if errors.As(err, &metaErr) {
		switch {
		case metaErr.IsRateLimit() || metaErr.IsTransient():
			// Throttling and Meta-side blips are transient: keep the profile
			// active, skip the telegram noise, and let asynq retry with backoff.
			log.Printf("sync profile %d transient meta error: %v", profile.ID, err)
			return err
		case metaErr.IsCheckpoint():
			alertType = "PROFILE_CHECKPOINT"
			status = "checkpoint"
		case metaErr.IsTokenError():
			alertType = "TOKEN_INVALID_OR_EXPIRED"
			status = "expired"
		}
	}
	if status != "" {
		if updateErr := p.store.UpdateProfileStatus(ctx, profile.ID, status, err.Error()); updateErr != nil {
			log.Printf("update profile status: %v", updateErr)
		}
	}

	text := fmt.Sprintf(
		"[ALERT] Meta tracking problem\n\nSocial account: %s (ID: %s)\nType: %s\nDescription: %s\nAction: tracking paused or retry required.",
		profile.FBName,
		profile.FBUserID,
		alertType,
		err.Error(),
	)
	var sentAt *time.Time
	if sendErr := p.telegram.SendAlert(ctx, text); sendErr != nil {
		log.Printf("telegram alert failed: %v", sendErr)
	} else if p.telegram.Enabled() {
		now := time.Now().UTC()
		sentAt = &now
	}
	profileID := profile.ID
	if createErr := p.store.CreateAlert(ctx, alertType, &profileID, nil, nil, err.Error(), map[string]any{
		"fb_user_id": profile.FBUserID,
		"fb_name":    profile.FBName,
	}, sentAt); createErr != nil {
		log.Printf("create alert: %v", createErr)
	}
	if status != "" {
		// Token is dead: retrying won't help until an operator reconnects the
		// profile, so stop asynq from hammering Meta.
		return fmt.Errorf("%v: %w", err, asynq.SkipRetry)
	}
	return err
}

// expiryAlertWindow is how far ahead of token expiry the scheduler warns, so an
// operator has time to reconnect the profile before sync breaks.
const expiryAlertWindow = 7 * 24 * time.Hour

type Scheduler struct {
	store       *repository.Store
	client      *asynq.Client
	metaClient  *meta.Client
	tokenCipher *crypto.TokenCipher
	telegram    *telegram.Client
	interval    time.Duration
}

func NewScheduler(store *repository.Store, client *asynq.Client, metaClient *meta.Client, tokenCipher *crypto.TokenCipher, telegramClient *telegram.Client, interval time.Duration) *Scheduler {
	return &Scheduler{store: store, client: client, metaClient: metaClient, tokenCipher: tokenCipher, telegram: telegramClient, interval: interval}
}

func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.enqueueOnce(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.enqueueOnce(ctx); err != nil {
				log.Printf("enqueue sync jobs: %v", err)
			}
		}
	}
}

func (s *Scheduler) enqueueOnce(ctx context.Context) error {
	s.alertExpiringTokens(ctx)

	now := time.Now().UTC()
	today := utcDate(now)
	profiles, err := s.store.ActiveProfiles(ctx)
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if needsDailyAccountResync(profile.LastAccountResyncDate, today) {
			if err := s.resyncProfileAccounts(ctx, profile, now); err != nil {
				log.Printf("daily account resync for profile %d: %v", profile.ID, err)
				continue
			}
		}
		if err := EnqueueSyncProfile(ctx, s.client, profile.ID, 0, s.interval); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) resyncProfileAccounts(ctx context.Context, profile repository.SyncProfile, now time.Time) error {
	token, err := s.tokenCipher.Decrypt(profile.AccessTokenCiphertext)
	if err != nil {
		_ = s.store.MarkAccountResyncFailure(ctx, profile.ID, err.Error())
		return err
	}
	accounts, err := s.metaClient.AdAccounts(ctx, token)
	if err != nil {
		_ = s.store.MarkAccountResyncFailure(ctx, profile.ID, err.Error())
		return err
	}
	if err := s.store.UpsertAdAccountsForProfile(ctx, profile.ID, accounts); err != nil {
		_ = s.store.MarkAccountResyncFailure(ctx, profile.ID, err.Error())
		return err
	}
	if err := s.store.MarkAccountResyncSuccess(ctx, profile.ID, now); err != nil {
		return err
	}
	log.Printf("daily account resync for profile %d: imported %d account(s)", profile.ID, len(accounts))
	return nil
}

func needsDailyAccountResync(lastDate *time.Time, today time.Time) bool {
	if lastDate == nil {
		return true
	}
	return !utcDate(lastDate.UTC()).Equal(today)
}

func utcDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// alertExpiringTokens warns once per profile when its long-lived token nears
// expiry. The alert is deduped in the store (token_expiry_alert_sent_at), so it
// fires a single time until the buyer reconnects the profile via OAuth.
func (s *Scheduler) alertExpiringTokens(ctx context.Context) {
	cutoff := time.Now().UTC().Add(expiryAlertWindow)
	profiles, err := s.store.ProfilesNeedingExpiryAlert(ctx, cutoff)
	if err != nil {
		log.Printf("query expiring tokens: %v", err)
		return
	}
	for _, profile := range profiles {
		text := fmt.Sprintf(
			"[ALERT] Meta token expiring soon\n\nSocial account: %s (ID: %s)\nExpires at: %s\nAction: reconnect the profile via OAuth before it expires.",
			profile.FBName,
			profile.FBUserID,
			profile.TokenExpiresAt.Format(time.RFC3339),
		)
		var sentAt *time.Time
		if sendErr := s.telegram.SendAlert(ctx, text); sendErr != nil {
			log.Printf("telegram expiry alert failed: %v", sendErr)
		} else if s.telegram.Enabled() {
			now := time.Now().UTC()
			sentAt = &now
		}
		profileID := profile.ID
		if createErr := s.store.CreateAlert(ctx, "TOKEN_EXPIRING_SOON", &profileID, nil, nil, "token expiring soon", map[string]any{
			"fb_user_id": profile.FBUserID,
			"fb_name":    profile.FBName,
			"expires_at": profile.TokenExpiresAt.Format(time.RFC3339),
		}, sentAt); createErr != nil {
			log.Printf("create expiry alert: %v", createErr)
		}
		if markErr := s.store.MarkExpiryAlerted(ctx, profile.ID); markErr != nil {
			log.Printf("mark expiry alerted: %v", markErr)
		}
	}
}

func RedisClientOpt(addr, password string, db int) asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: addr, Password: password, DB: db}
}
