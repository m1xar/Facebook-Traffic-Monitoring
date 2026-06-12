package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
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
}

func NewProcessor(store *repository.Store, metaClient *meta.Client, tokenCipher *crypto.TokenCipher, telegramClient *telegram.Client) *Processor {
	return &Processor{
		store:       store,
		metaClient:  metaClient,
		tokenCipher: tokenCipher,
		telegram:    telegramClient,
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

// EnqueueSyncProfile enqueues a sync task for one profile. Uniqueness spans
// just under the sync interval, so an immediate post-connect sync and the
// scheduler can never stack duplicate tasks for the same profile.
func EnqueueSyncProfile(ctx context.Context, client *asynq.Client, profileID int64, delay, syncInterval time.Duration) error {
	task, err := NewSyncProfileTask(profileID)
	if err != nil {
		return err
	}
	_, err = client.EnqueueContext(ctx, task,
		asynq.ProcessIn(delay), asynq.MaxRetry(5), asynq.Timeout(10*time.Minute), asynq.Unique(uniqueWindow(syncInterval)))
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

// uniqueWindow is the task-uniqueness TTL: just under the sync interval
// (1h -> 55m), so the next scheduler tick is never blocked by its own lock.
func uniqueWindow(syncInterval time.Duration) time.Duration {
	return syncInterval - syncInterval/12
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

	accounts, err := p.store.TrackedPrimaryAccountsForProfile(ctx, profile.ID)
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
	store    *repository.Store
	client   *asynq.Client
	telegram *telegram.Client
	interval time.Duration
}

func NewScheduler(store *repository.Store, client *asynq.Client, telegramClient *telegram.Client, interval time.Duration) *Scheduler {
	return &Scheduler{store: store, client: client, telegram: telegramClient, interval: interval}
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

	profiles, err := s.store.ActiveProfiles(ctx)
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		// Jitter inside [interval/12, interval/6] (1h -> 5-10 min) so
		// profiles do not hit Meta at the same instant every tick.
		minDelay := s.interval / 12
		delay := minDelay + time.Duration(rand.Int63n(int64(minDelay)+1))
		if err := EnqueueSyncProfile(ctx, s.client, profile.ID, delay, s.interval); err != nil {
			return err
		}
	}
	return nil
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
