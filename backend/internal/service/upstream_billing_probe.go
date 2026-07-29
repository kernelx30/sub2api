package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	// These values live in accounts.extra so PR2 does not require a schema migration.
	UpstreamBillingProbeExtraKey        = "upstream_billing_probe"
	UpstreamBillingProbeEnabledExtraKey = "upstream_billing_probe_enabled"
	PoolAutoPriorityEnabledExtraKey     = "pool_auto_priority_enabled"

	upstreamBillingProbeDefaultIntervalMinutes = 5
	upstreamBillingProbeMinIntervalMinutes     = 5
	upstreamBillingProbeMaxIntervalMinutes     = 24 * 60
	upstreamBillingProbeCycleInterval          = time.Minute
	upstreamBillingProbeRequestTimeout         = 10 * time.Second
	upstreamBillingProbeMaxBodyBytes           = 64 * 1024
	upstreamBillingProbeMaxPerCycle            = 20
	upstreamBillingProbeConcurrency            = 4
	upstreamBillingProbeMaxDelay               = 24 * time.Hour
	upstreamBillingProbeLeaderLockKey          = "upstream:billing:probe:leader"
	upstreamBillingProbeLeaderLockTTL          = 2 * time.Minute
	poolAutoPriorityDefaultIntervalMinutes     = 5
	poolAutoPriorityMinIntervalMinutes         = 5
	poolAutoPriorityMaxIntervalMinutes         = 60
)

// UpstreamBillingProbeMaxBatchSize limits one manual batch and one runner cycle.
const UpstreamBillingProbeMaxBatchSize = upstreamBillingProbeMaxPerCycle

var (
	ErrUpstreamBillingProbeUnavailable = infraerrors.ServiceUnavailable(
		"UPSTREAM_BILLING_PROBE_UNAVAILABLE", "upstream billing probe is unavailable",
	)
	ErrUpstreamBillingProbeAccountInvalid = infraerrors.BadRequest(
		"UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID", "account is not an OpenAI API key account",
	)
	ErrUpstreamBillingProbeIdentityChanged = infraerrors.Conflict(
		"UPSTREAM_BILLING_PROBE_IDENTITY_CHANGED", "account identity changed during upstream billing probe; retry the probe",
	)
)

const (
	UpstreamBillingProbeStatusOK          = "ok"
	UpstreamBillingProbeStatusUnsupported = "unsupported"
	UpstreamBillingProbeStatusFailed      = "failed"
)

// UpstreamBillingProbeSettings controls the periodic probe runner.
type UpstreamBillingProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// PoolAutoPrioritySettings controls model health probes used only by pool-mode
// runtime ordering. It is intentionally independent from billing discovery.
type PoolAutoPrioritySettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// UpstreamBillingProbeSnapshot is persisted in accounts.extra. Data is kept as
// a sanitized map so future response fields do not require a database change.
type UpstreamBillingProbeSnapshot struct {
	Status                  string         `json:"status"`
	BillingProbeAttempted   bool           `json:"billing_probe_attempted,omitempty"`
	Data                    map[string]any `json:"data,omitempty"`
	ReceivedAt              *time.Time     `json:"received_at,omitempty"`
	FreshUntil              *time.Time     `json:"fresh_until,omitempty"`
	LastAttemptAt           time.Time      `json:"last_attempt_at"`
	NextProbeAt             time.Time      `json:"next_probe_at"`
	FailureCount            int            `json:"failure_count,omitempty"`
	HTTPStatus              int            `json:"http_status,omitempty"`
	LatencyMS               int64          `json:"latency_ms,omitempty"`
	ModelProbeStatus        string         `json:"model_probe_status,omitempty"`
	ModelProbeModel         string         `json:"model_probe_model,omitempty"`
	ModelProbeEndpoint      string         `json:"model_probe_endpoint,omitempty"`
	ModelProbeLatencyMS     int64          `json:"model_probe_latency_ms,omitempty"`
	ModelProbeHTTPStatus    int            `json:"model_probe_http_status,omitempty"`
	ModelProbeLastError     string         `json:"model_probe_last_error,omitempty"`
	ModelProbeLastAttemptAt *time.Time     `json:"model_probe_last_attempt_at,omitempty"`
	ModelProbeFreshUntil    *time.Time     `json:"model_probe_fresh_until,omitempty"`
	ModelProbeNextAt        *time.Time     `json:"model_probe_next_at,omitempty"`
	ModelProbeFailureCount  int            `json:"model_probe_failure_count,omitempty"`
	LastError               string         `json:"last_error,omitempty"`
}

// UpstreamBillingProbeResult is returned by manual probe endpoints.
type UpstreamBillingProbeResult struct {
	AccountID int64                         `json:"account_id"`
	Snapshot  *UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
	Error     string                        `json:"error,omitempty"`
}

type upstreamBillingProbeResponse struct {
	Object                  string   `json:"object"`
	SchemaVersion           int      `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64 `json:"resolved_rate_multiplier"`
	PeakRateEnabled         *bool    `json:"peak_rate_enabled"`
	PeakStart               *string  `json:"peak_start"`
	PeakEnd                 *string  `json:"peak_end"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	Timezone                *string  `json:"timezone"`
	ObservedAt              string   `json:"observed_at"`
}

// GetUpstreamBillingProbeSettings returns defaults when the setting is absent.
func (s *SettingService) GetUpstreamBillingProbeSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	defaults := defaultUpstreamBillingProbeSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyUpstreamBillingProbeSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get upstream billing probe settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse upstream billing probe settings: %w", err)
	}
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	normalizeUpstreamBillingProbeSettings(&settings)
	return &settings, nil
}

// SetUpstreamBillingProbeSettings validates and persists the runner settings.
func (s *SettingService) SetUpstreamBillingProbeSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if settings == nil {
		return infraerrors.BadRequest("INVALID_UPSTREAM_BILLING_PROBE_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < upstreamBillingProbeMinIntervalMinutes || settings.IntervalMinutes > upstreamBillingProbeMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_UPSTREAM_BILLING_PROBE_INTERVAL",
			fmt.Sprintf("interval_minutes must be between %d and %d", upstreamBillingProbeMinIntervalMinutes, upstreamBillingProbeMaxIntervalMinutes),
		)
	}
	normalizeUpstreamBillingProbeSettings(settings)
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal upstream billing probe settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyUpstreamBillingProbeSettings, string(data))
}

func defaultUpstreamBillingProbeSettings() *UpstreamBillingProbeSettings {
	return &UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: upstreamBillingProbeDefaultIntervalMinutes}
}

func normalizeUpstreamBillingProbeSettings(settings *UpstreamBillingProbeSettings) {
	if settings.IntervalMinutes < upstreamBillingProbeMinIntervalMinutes {
		settings.IntervalMinutes = upstreamBillingProbeMinIntervalMinutes
	}
	if settings.IntervalMinutes > upstreamBillingProbeMaxIntervalMinutes {
		settings.IntervalMinutes = upstreamBillingProbeMaxIntervalMinutes
	}
}

// GetPoolAutoPrioritySettings returns the independent pool ordering settings.
func (s *SettingService) GetPoolAutoPrioritySettings(ctx context.Context) (*PoolAutoPrioritySettings, error) {
	defaults := defaultPoolAutoPrioritySettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyPoolAutoPrioritySettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return defaults, nil
		}
		return nil, fmt.Errorf("get pool auto priority settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return defaults, nil
	}
	settings := *defaults
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return nil, fmt.Errorf("parse pool auto priority settings: %w", err)
	}
	if settings.IntervalMinutes == 0 {
		settings.IntervalMinutes = defaults.IntervalMinutes
	}
	normalizePoolAutoPrioritySettings(&settings)
	return &settings, nil
}

func (s *SettingService) SetPoolAutoPrioritySettings(ctx context.Context, settings *PoolAutoPrioritySettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting repository is unavailable")
	}
	if settings == nil {
		return infraerrors.BadRequest("INVALID_POOL_AUTO_PRIORITY_SETTINGS", "settings cannot be nil")
	}
	if settings.IntervalMinutes < poolAutoPriorityMinIntervalMinutes || settings.IntervalMinutes > poolAutoPriorityMaxIntervalMinutes {
		return infraerrors.BadRequest(
			"INVALID_POOL_AUTO_PRIORITY_INTERVAL",
			fmt.Sprintf("interval_minutes must be between %d and %d", poolAutoPriorityMinIntervalMinutes, poolAutoPriorityMaxIntervalMinutes),
		)
	}
	normalizePoolAutoPrioritySettings(settings)
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal pool auto priority settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyPoolAutoPrioritySettings, string(data))
}

func defaultPoolAutoPrioritySettings() *PoolAutoPrioritySettings {
	return &PoolAutoPrioritySettings{Enabled: true, IntervalMinutes: poolAutoPriorityDefaultIntervalMinutes}
}

func normalizePoolAutoPrioritySettings(settings *PoolAutoPrioritySettings) {
	if settings.IntervalMinutes < poolAutoPriorityMinIntervalMinutes {
		settings.IntervalMinutes = poolAutoPriorityMinIntervalMinutes
	}
	if settings.IntervalMinutes > poolAutoPriorityMaxIntervalMinutes {
		settings.IntervalMinutes = poolAutoPriorityMaxIntervalMinutes
	}
}

// UpstreamBillingProbeService discovers a remote Sub2API billing snapshot.
type UpstreamBillingProbeService struct {
	accountRepo        AccountRepository
	accountTestService *AccountTestService
	settingService     *SettingService

	parentCtx    context.Context
	parentCancel context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	started      bool
	stopped      bool
	cycleMu      sync.Mutex
	probeGroup   singleflight.Group
	probeSlots   chan struct{}
	now          func() time.Time
	lockCache    LeaderLockCache
	db           *sql.DB
	instanceID   string
}

type upstreamBillingProbeSnapshotWriter interface {
	UpdateUpstreamBillingProbeSnapshot(context.Context, *Account, *UpstreamBillingProbeSnapshot) error
}

type upstreamBillingProbeDueAccountLister interface {
	ListDueUpstreamBillingProbeAccounts(context.Context, time.Time, bool, bool, int) ([]Account, error)
}

func NewUpstreamBillingProbeService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	settingService *SettingService,
) *UpstreamBillingProbeService {
	ctx, cancel := context.WithCancel(context.Background())
	return &UpstreamBillingProbeService{
		accountRepo:        accountRepo,
		accountTestService: accountTestService,
		settingService:     settingService,
		parentCtx:          ctx,
		parentCancel:       cancel,
		probeSlots:         make(chan struct{}, upstreamBillingProbeConcurrency),
		now:                time.Now,
		instanceID:         uuid.NewString(),
	}
}

func (s *UpstreamBillingProbeService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ProvideUpstreamBillingProbeService starts the process-wide periodic runner.
func ProvideUpstreamBillingProbeService(
	accountRepo AccountRepository,
	accountTestService *AccountTestService,
	settingService *SettingService,
	lockCache LeaderLockCache,
	db *sql.DB,
) *UpstreamBillingProbeService {
	svc := NewUpstreamBillingProbeService(accountRepo, accountTestService, settingService)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

func (s *UpstreamBillingProbeService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *UpstreamBillingProbeService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *UpstreamBillingProbeService) runLoop() {
	defer s.wg.Done()
	_ = s.RunDue(s.parentCtx)
	ticker := time.NewTicker(upstreamBillingProbeCycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if err := s.RunDue(s.parentCtx); err != nil {
				logger.LegacyPrintf("service.upstream_billing_probe", "run_due_failed: err=%v", err)
			}
		}
	}
}

// RunDue executes at most one bounded batch of due accounts.
func (s *UpstreamBillingProbeService) RunDue(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	settings, err := s.getSettings(ctx)
	if err != nil {
		return err
	}
	poolSettings, err := s.getPoolAutoPrioritySettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled && !poolSettings.Enabled {
		return nil
	}
	runRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, upstreamBillingProbeLeaderLockKey)
	if lockErr != nil {
		return fmt.Errorf("acquire upstream billing probe leader lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer runRelease()

	lockNow := time.Now()
	cadenceRelease, acquired, lockErr := s.tryAcquireLeaderLock(ctx, upstreamBillingProbeLeaderLockKeyAt(lockNow))
	if lockErr != nil {
		return fmt.Errorf("acquire upstream billing probe cadence lock: %w", lockErr)
	}
	if !acquired {
		return nil
	}
	defer releaseUpstreamBillingProbeLeaderLock(cadenceRelease, lockNow.Truncate(upstreamBillingProbeCycleInterval).Add(upstreamBillingProbeCycleInterval))

	now := s.currentTime()
	accounts, err := s.listDueAccounts(ctx, now, settings.Enabled, poolSettings.Enabled)
	if err != nil {
		return fmt.Errorf("list enabled upstream billing probes: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		billingEnabled, priorityEnabled := scheduledProbeModes(&account, settings.Enabled, poolSettings.Enabled)
		if !isUpstreamBillingProbeAccount(&account) || !account.IsActive() || (!billingEnabled && !priorityEnabled) {
			continue
		}
		snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
		if snapshot != nil && !snapshot.NextProbeAt.IsZero() && now.Before(snapshot.NextProbeAt) {
			continue
		}
		due = append(due, account)
	}
	sort.SliceStable(due, func(i, j int) bool {
		left := decodeUpstreamBillingProbeSnapshot(due[i].Extra)
		right := decodeUpstreamBillingProbeSnapshot(due[j].Extra)
		leftUnset := left == nil || left.NextProbeAt.IsZero()
		rightUnset := right == nil || right.NextProbeAt.IsZero()
		if leftUnset && rightUnset {
			return due[i].ID < due[j].ID
		}
		if leftUnset {
			return true
		}
		if rightUnset {
			return false
		}
		return left.NextProbeAt.Before(right.NextProbeAt)
	})
	if len(due) > upstreamBillingProbeMaxPerCycle {
		due = due[:upstreamBillingProbeMaxPerCycle]
	}

	var group errgroup.Group
	for i := range due {
		accountID := due[i].ID
		group.Go(func() error {
			if _, probeErr := s.probeScheduledAccount(ctx, accountID, settings, poolSettings); probeErr != nil {
				logger.LegacyPrintf("service.upstream_billing_probe", "probe_due_failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

func (s *UpstreamBillingProbeService) listDueAccounts(ctx context.Context, now time.Time, includeBilling, includePoolPriority bool) ([]Account, error) {
	if lister, ok := s.accountRepo.(upstreamBillingProbeDueAccountLister); ok {
		return lister.ListDueUpstreamBillingProbeAccounts(ctx, now, includeBilling, includePoolPriority, upstreamBillingProbeMaxPerCycle)
	}
	// Non-production repositories and older adapters load OpenAI accounts, then
	// RunDue applies the same explicit-switch-or-pool-mode eligibility rule.
	return s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
}

func (s *UpstreamBillingProbeService) getSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	if s.settingService == nil {
		return defaultUpstreamBillingProbeSettings(), nil
	}
	return s.settingService.GetUpstreamBillingProbeSettings(ctx)
}

func (s *UpstreamBillingProbeService) getPoolAutoPrioritySettings(ctx context.Context) (*PoolAutoPrioritySettings, error) {
	if s.settingService == nil {
		return defaultPoolAutoPrioritySettings(), nil
	}
	return s.settingService.GetPoolAutoPrioritySettings(ctx)
}

func (s *UpstreamBillingProbeService) GetSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	return s.getSettings(ctx)
}

func (s *UpstreamBillingProbeService) UpdateSettings(ctx context.Context, settings *UpstreamBillingProbeSettings) error {
	if s == nil || s.settingService == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	return s.settingService.SetUpstreamBillingProbeSettings(ctx, settings)
}

func (s *UpstreamBillingProbeService) GetPoolAutoPrioritySettings(ctx context.Context) (*PoolAutoPrioritySettings, error) {
	return s.getPoolAutoPrioritySettings(ctx)
}

func (s *UpstreamBillingProbeService) UpdatePoolAutoPrioritySettings(ctx context.Context, settings *PoolAutoPrioritySettings) error {
	if s == nil || s.settingService == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	return s.settingService.SetPoolAutoPrioritySettings(ctx, settings)
}

// ProbeAccount performs one manual or scheduled probe. Manual calls ignore both switches.
func (s *UpstreamBillingProbeService) ProbeAccount(ctx context.Context, accountID int64) (*UpstreamBillingProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	settings, err := s.getSettings(ctx)
	if err != nil {
		return nil, err
	}
	poolSettings, err := s.getPoolAutoPrioritySettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.probeAccount(ctx, accountID, settings, poolSettings)
}

func (s *UpstreamBillingProbeService) probeAccount(ctx context.Context, accountID int64, settings *UpstreamBillingProbeSettings, poolSettings *PoolAutoPrioritySettings) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, settings, poolSettings, false)
}

func (s *UpstreamBillingProbeService) probeScheduledAccount(ctx context.Context, accountID int64, settings *UpstreamBillingProbeSettings, poolSettings *PoolAutoPrioritySettings) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, settings, poolSettings, true)
}

func (s *UpstreamBillingProbeService) probeAccountWithMode(ctx context.Context, accountID int64, settings *UpstreamBillingProbeSettings, poolSettings *PoolAutoPrioritySettings, requireEnabled bool) (*UpstreamBillingProbeSnapshot, error) {
	key := strconv.FormatInt(accountID, 10)
	value, err, _ := s.probeGroup.Do(key, func() (any, error) {
		select {
		case s.probeSlots <- struct{}{}:
			defer func() { <-s.probeSlots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		account, loadErr := s.accountRepo.GetByID(ctx, accountID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !isUpstreamBillingProbeAccount(account) {
			return nil, ErrUpstreamBillingProbeAccountInvalid
		}
		if settings == nil {
			settings = defaultUpstreamBillingProbeSettings()
		}
		if poolSettings == nil {
			poolSettings = defaultPoolAutoPrioritySettings()
		}
		includeBilling := true
		includeModel := account.IsPoolMode()
		if requireEnabled {
			includeBilling, includeModel = scheduledProbeModes(account, settings.Enabled, poolSettings.Enabled)
			if !account.IsActive() || (!includeBilling && !includeModel) {
				return nil, nil
			}
			if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil &&
				!snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
				return nil, nil
			}
		}
		intervalMinutes := effectiveProbeIntervalMinutes(includeBilling, includeModel, settings.IntervalMinutes, poolSettings.IntervalMinutes)
		return s.probeLoadedAccount(ctx, account, intervalMinutes, includeBilling, includeModel)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	snapshot, ok := value.(*UpstreamBillingProbeSnapshot)
	if !ok {
		return nil, fmt.Errorf("invalid upstream billing probe result")
	}
	return snapshot, nil
}

// ProbeAccounts performs a bounded manual batch with the same concurrency limit as the runner.
func (s *UpstreamBillingProbeService) ProbeAccounts(ctx context.Context, accountIDs []int64) []UpstreamBillingProbeResult {
	if len(accountIDs) > upstreamBillingProbeMaxPerCycle {
		accountIDs = accountIDs[:upstreamBillingProbeMaxPerCycle]
	}
	results := make([]UpstreamBillingProbeResult, len(accountIDs))
	if s == nil || s.accountRepo == nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBillingProbeResult{AccountID: accountID, Error: ErrUpstreamBillingProbeUnavailable.Error()}
		}
		return results
	}
	settings, settingsErr := s.getSettings(ctx)
	if settingsErr != nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBillingProbeResult{AccountID: accountID, Error: safeProbeError(settingsErr)}
		}
		return results
	}
	poolSettings, settingsErr := s.getPoolAutoPrioritySettings(ctx)
	if settingsErr != nil {
		for i, accountID := range accountIDs {
			results[i] = UpstreamBillingProbeResult{AccountID: accountID, Error: safeProbeError(settingsErr)}
		}
		return results
	}
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.probeAccount(ctx, accountID, settings, poolSettings)
			if err != nil {
				results[i].Error = safeProbeError(err)
				return nil
			}
			results[i].Snapshot = snapshot
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func upstreamBillingProbeLeaderLockKeyAt(now time.Time) string {
	return fmt.Sprintf("%s:%d", upstreamBillingProbeLeaderLockKey, now.Unix()/int64(upstreamBillingProbeCycleInterval/time.Second))
}

func (s *UpstreamBillingProbeService) tryAcquireLeaderLock(ctx context.Context, key string) (func(), bool, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.lockCache != nil {
		acquired, err := s.lockCache.TryAcquireLeaderLock(lockCtx, key, s.instanceID, upstreamBillingProbeLeaderLockTTL)
		if err != nil {
			return nil, false, err
		}
		if !acquired {
			return nil, false, nil
		}
		return func() {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer releaseCancel()
			_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
		}, true, nil
	}
	if s.db != nil {
		return tryAcquireDBAdvisoryLockWithError(lockCtx, s.db, hashAdvisoryLockID(key))
	}
	return func() {}, true, nil
}

func releaseUpstreamBillingProbeLeaderLock(release func(), releaseAt time.Time) {
	delay := time.Until(releaseAt)
	if delay <= 0 {
		release()
		return
	}
	time.AfterFunc(delay, release)
}

func (s *UpstreamBillingProbeService) SetAccountEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return ErrUpstreamBillingProbeAccountInvalid
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		UpstreamBillingProbeEnabledExtraKey: enabled,
	})
}

func (s *UpstreamBillingProbeService) SetPoolAutoPriorityEnabled(ctx context.Context, accountID int64, enabled bool) error {
	if s == nil || s.accountRepo == nil {
		return ErrUpstreamBillingProbeUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !isUpstreamBillingProbeAccount(account) {
		return ErrUpstreamBillingProbeAccountInvalid
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		PoolAutoPriorityEnabledExtraKey: enabled,
	})
}

type upstreamModelProbeResult struct {
	Status     string
	Model      string
	Endpoint   string
	LatencyMS  int64
	HTTPStatus int
	LastError  string
}

func (s *UpstreamBillingProbeService) probeLoadedAccount(ctx context.Context, account *Account, intervalMinutes int, includeBilling, includeModel bool) (*UpstreamBillingProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "transport_unavailable", includeBilling, includeModel)
	}
	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "missing_api_key", includeBilling, includeModel)
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	normalizedBaseURL, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "invalid_base_url", includeBilling, includeModel)
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "proxy_unavailable", includeBilling, includeModel)
		}
		if account.Proxy.ID != *account.ProxyID {
			return nil, ErrUpstreamBillingProbeIdentityChanged
		}
		proxyURL = account.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if s.accountTestService.tlsFPProfileService != nil {
		tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
	}

	// Pool-mode routing needs real model reachability. Ordinary channel-status
	// probes keep the original billing-only behavior to avoid unnecessary spend.
	var modelProbe *upstreamModelProbeResult
	if includeModel {
		modelProbe = s.probeUpstreamModel(ctx, account, normalizedBaseURL, apiKey, proxyURL, tlsProfile)
	}
	if !includeBilling {
		return s.persistModelOnlyProbe(ctx, account, intervalMinutes, now, modelProbe)
	}

	probeURL := buildOpenAIEndpointURL(normalizedBaseURL, "/v1/sub2api/billing")
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, bytes.NewReader(nil))
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "request_build_failed", 0, modelProbe)
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	probeStarted := time.Now()
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "request_failed", 0, modelProbe)
	}
	if resp == nil || resp.Body == nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "empty_response", 0, modelProbe)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamBillingProbeMaxBodyBytes+1))
	latencyMS := probeLatencyMS(probeStarted)
	if readErr != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_read_failed", retryAfter(resp.Header, now), modelProbe)
	}
	if len(body) > upstreamBillingProbeMaxBodyBytes {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_too_large", retryAfter(resp.Header, now), modelProbe)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "unsupported", retryAfter(resp.Header, now), modelProbe)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "http_error", retryAfter(resp.Header, now), modelProbe)
	}
	data, err := parseUpstreamBillingProbeResponse(body)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "invalid_response", retryAfter(resp.Header, now), modelProbe)
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:                UpstreamBillingProbeStatusOK,
		BillingProbeAttempted: true,
		Data:                  data,
		ReceivedAt:            probeTimePtr(now),
		FreshUntil:            probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute)),
		LastAttemptAt:         now,
		NextProbeAt:           now.Add(nextProbeDelay(intervalMinutes, 0)),
		HTTPStatus:            resp.StatusCode,
		LatencyMS:             latencyMS,
	}
	applyScheduledUpstreamModelProbe(snapshot, account, modelProbe, now, intervalMinutes, 0)
	if snapshot.ModelProbeNextAt != nil && snapshot.ModelProbeNextAt.Before(snapshot.NextProbeAt) {
		snapshot.NextProbeAt = *snapshot.ModelProbeNextAt
	}
	if err := s.updateSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) probeUpstreamModel(
	ctx context.Context,
	account *Account,
	normalizedBaseURL string,
	apiKey string,
	proxyURL string,
	tlsProfile *tlsfingerprint.Profile,
) *upstreamModelProbeResult {
	result := &upstreamModelProbeResult{
		Status: UpstreamBillingProbeStatusFailed,
		Model:  selectUpstreamHealthProbeModel(account),
	}
	useResponses := openai_compat.ShouldUseResponsesAPI(account.Extra)
	var payload []byte
	if useResponses {
		result.Endpoint = "responses"
		payload = openaiResponsesProbePayload(result.Model)
	} else {
		result.Endpoint = "chat_completions"
		payload, _ = json.Marshal(createOpenAIChatCompletionsTestPayload(result.Model, "hi"))
	}

	probeURL := buildOpenAIResponsesURL(normalizedBaseURL)
	accept := "application/json"
	if !useResponses {
		probeURL = buildOpenAIChatCompletionsURL(normalizedBaseURL)
		accept = "text/event-stream"
	}
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, probeURL, bytes.NewReader(payload))
	if err != nil {
		result.LastError = "request_build_failed"
		return result
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if useResponses {
		applyOpenAICodexProbeHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)

	started := time.Now()
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		result.LatencyMS = probeLatencyMS(started)
		result.LastError = "request_failed"
		return result
	}
	if resp == nil || resp.Body == nil {
		result.LatencyMS = probeLatencyMS(started)
		result.LastError = "empty_response"
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesProbeMaxBodyBytes+1))
	result.LatencyMS = probeLatencyMS(started)
	result.HTTPStatus = resp.StatusCode
	if readErr != nil {
		result.LastError = "response_read_failed"
		return result
	}
	if len(body) > responsesProbeMaxBodyBytes {
		result.LastError = "response_too_large"
		return result
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		result.Status = UpstreamBillingProbeStatusUnsupported
		result.LastError = "unsupported"
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.LastError = "http_error"
		return result
	}
	if useResponses && !decideResponsesProbeSupport(resp.StatusCode, body) {
		result.Status = UpstreamBillingProbeStatusUnsupported
		result.LastError = "responses_tool_unsupported"
		return result
	}
	if len(body) == 0 {
		result.LastError = "empty_response"
		return result
	}
	result.Status = UpstreamBillingProbeStatusOK
	result.LastError = ""
	return result
}

func selectUpstreamHealthProbeModel(account *Account) string {
	if account == nil {
		return "gpt-5.6-terra"
	}
	mapping := account.GetModelMapping()
	for _, requested := range []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6",
		"gpt-5.5",
		"gpt-5.4",
		"codex-auto-review",
	} {
		upstream := strings.TrimSpace(mapping[requested])
		if upstream != "" && !strings.Contains(upstream, "*") {
			return upstream
		}
	}
	return selectResponsesProbeModel(account)
}

func applyUpstreamModelProbe(snapshot *UpstreamBillingProbeSnapshot, result *upstreamModelProbeResult) {
	if snapshot == nil || result == nil {
		return
	}
	snapshot.ModelProbeStatus = result.Status
	snapshot.ModelProbeModel = result.Model
	snapshot.ModelProbeEndpoint = result.Endpoint
	snapshot.ModelProbeLatencyMS = result.LatencyMS
	snapshot.ModelProbeHTTPStatus = result.HTTPStatus
	snapshot.ModelProbeLastError = result.LastError
}

func applyScheduledUpstreamModelProbe(snapshot *UpstreamBillingProbeSnapshot, account *Account, result *upstreamModelProbeResult, now time.Time, intervalMinutes int, retryAfterDuration time.Duration) {
	if snapshot == nil || result == nil {
		return
	}
	applyUpstreamModelProbe(snapshot, result)
	snapshot.ModelProbeLastAttemptAt = probeTimePtr(now)
	failureCount := 0
	delay := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if result.Status == UpstreamBillingProbeStatusOK {
		snapshot.ModelProbeFreshUntil = probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute))
	} else {
		failureCount = nextModelProbeFailureCount(account)
		delay = nextProbeFailureDelay(intervalMinutes, failureCount, retryAfterDuration)
		snapshot.ModelProbeFreshUntil = nil
	}
	snapshot.ModelProbeFailureCount = failureCount
	snapshot.ModelProbeNextAt = probeTimePtr(now.Add(delay))
}

func nextProbeFailureCount(account *Account) int {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	if previous == nil || previous.FailureCount < 1 {
		return 1
	}
	return previous.FailureCount + 1
}

func nextModelProbeFailureCount(account *Account) int {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	if previous == nil || previous.ModelProbeFailureCount < 1 {
		return 1
	}
	return previous.ModelProbeFailureCount + 1
}

func failedUpstreamModelProbe(account *Account, reason string) *upstreamModelProbeResult {
	result := &upstreamModelProbeResult{
		Status:    UpstreamBillingProbeStatusFailed,
		Model:     selectUpstreamHealthProbeModel(account),
		LastError: reason,
	}
	if openai_compat.ShouldUseResponsesAPI(account.Extra) {
		result.Endpoint = "responses"
	} else {
		result.Endpoint = "chat_completions"
	}
	return result
}

func (s *UpstreamBillingProbeService) persistPlannedProbeFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	reason string,
	includeBilling bool,
	includeModel bool,
) (*UpstreamBillingProbeSnapshot, error) {
	var modelProbe *upstreamModelProbeResult
	if includeModel {
		modelProbe = failedUpstreamModelProbe(account, reason)
	}
	if includeBilling {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, reason, 0, modelProbe)
	}
	return s.persistModelOnlyProbe(ctx, account, intervalMinutes, now, modelProbe)
}

func (s *UpstreamBillingProbeService) persistModelOnlyProbe(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	modelProbe *upstreamModelProbeResult,
) (*UpstreamBillingProbeSnapshot, error) {
	if modelProbe == nil {
		modelProbe = failedUpstreamModelProbe(account, "model_probe_unavailable")
	}
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	snapshot := &UpstreamBillingProbeSnapshot{}
	if previous != nil {
		*snapshot = *previous
	}
	applyScheduledUpstreamModelProbe(snapshot, account, modelProbe, now, intervalMinutes, 0)
	snapshot.LastAttemptAt = now
	if snapshot.ModelProbeNextAt != nil {
		snapshot.NextProbeAt = *snapshot.ModelProbeNextAt
	}
	if snapshot.Status == "" {
		snapshot.Status = modelProbe.Status
	}
	if snapshot.Data == nil {
		snapshot.FailureCount = snapshot.ModelProbeFailureCount
		snapshot.LastError = modelProbe.LastError
	}
	if err := s.updateSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) persistProbeFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
	retryAfterDuration time.Duration,
	modelProbe *upstreamModelProbeResult,
) (*UpstreamBillingProbeSnapshot, error) {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	failureCount := nextProbeFailureCount(account)
	status := UpstreamBillingProbeStatusFailed
	if reason == "unsupported" {
		status = UpstreamBillingProbeStatusUnsupported
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:                status,
		BillingProbeAttempted: true,
		LastAttemptAt:         now,
		NextProbeAt:           now.Add(nextProbeFailureDelay(intervalMinutes, failureCount, retryAfterDuration)),
		FailureCount:          failureCount,
		HTTPStatus:            statusCode,
		LastError:             reason,
	}
	if previous != nil {
		snapshot.Data = previous.Data
		snapshot.ReceivedAt = previous.ReceivedAt
		snapshot.FreshUntil = previous.FreshUntil
		if snapshot.FreshUntil == nil && previous.Status == UpstreamBillingProbeStatusOK && previous.ReceivedAt != nil {
			snapshot.FreshUntil = probeTimePtr(previous.ReceivedAt.Add(2 * time.Duration(intervalMinutes) * time.Minute))
		}
	}
	applyScheduledUpstreamModelProbe(snapshot, account, modelProbe, now, intervalMinutes, retryAfterDuration)
	if snapshot.ModelProbeNextAt != nil && snapshot.ModelProbeNextAt.Before(snapshot.NextProbeAt) {
		snapshot.NextProbeAt = *snapshot.ModelProbeNextAt
	}
	if err := s.updateSnapshot(ctx, account, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) updateSnapshot(ctx context.Context, account *Account, snapshot *UpstreamBillingProbeSnapshot) error {
	writer, ok := s.accountRepo.(upstreamBillingProbeSnapshotWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	return writer.UpdateUpstreamBillingProbeSnapshot(ctx, account, snapshot)
}

func parseUpstreamBillingProbeResponse(body []byte) (map[string]any, error) {
	var response upstreamBillingProbeResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Object != "sub2api.key_billing" || response.SchemaVersion != 1 || response.BillingScope != "token" {
		return nil, fmt.Errorf("unexpected billing response schema")
	}
	if response.GroupRateMultiplier == nil || response.ResolvedRateMultiplier == nil ||
		response.PeakRateEnabled == nil || response.EffectiveRateMultiplier == nil {
		return nil, fmt.Errorf("incomplete billing response")
	}
	for _, value := range []float64{
		*response.GroupRateMultiplier,
		*response.ResolvedRateMultiplier,
		*response.EffectiveRateMultiplier,
	} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid billing multiplier")
		}
	}
	if response.UserRateMultiplier != nil && (*response.UserRateMultiplier < 0 || math.IsNaN(*response.UserRateMultiplier) || math.IsInf(*response.UserRateMultiplier, 0)) {
		return nil, fmt.Errorf("invalid user billing multiplier")
	}
	expectedResolved := *response.GroupRateMultiplier
	if response.UserRateMultiplier != nil {
		expectedResolved = *response.UserRateMultiplier
	}
	if !equalBillingMultiplier(*response.ResolvedRateMultiplier, expectedResolved) {
		return nil, fmt.Errorf("inconsistent resolved billing multiplier")
	}
	observedAt, err := time.Parse(time.RFC3339Nano, response.ObservedAt)
	if err != nil || observedAt.IsZero() {
		return nil, fmt.Errorf("invalid observed_at")
	}
	data := map[string]any{
		"object":                    response.Object,
		"schema_version":            response.SchemaVersion,
		"billing_scope":             response.BillingScope,
		"group_rate_multiplier":     *response.GroupRateMultiplier,
		"resolved_rate_multiplier":  *response.ResolvedRateMultiplier,
		"peak_rate_enabled":         *response.PeakRateEnabled,
		"effective_rate_multiplier": *response.EffectiveRateMultiplier,
		"observed_at":               observedAt.UTC().Format(time.RFC3339Nano),
	}
	if response.UserRateMultiplier != nil {
		data["user_rate_multiplier"] = *response.UserRateMultiplier
	}
	if *response.PeakRateEnabled {
		if response.PeakStart == nil || response.PeakEnd == nil || response.Timezone == nil ||
			response.PeakRateMultiplier == nil || response.AppliedPeakMultiplier == nil ||
			*response.PeakStart == "" || *response.PeakEnd == "" || *response.Timezone == "" ||
			*response.PeakRateMultiplier < 0 || *response.AppliedPeakMultiplier < 0 ||
			math.IsNaN(*response.PeakRateMultiplier) || math.IsInf(*response.PeakRateMultiplier, 0) ||
			math.IsNaN(*response.AppliedPeakMultiplier) || math.IsInf(*response.AppliedPeakMultiplier, 0) {
			return nil, fmt.Errorf("incomplete peak billing response")
		}
		data["peak_start"] = *response.PeakStart
		data["peak_end"] = *response.PeakEnd
		data["peak_rate_multiplier"] = *response.PeakRateMultiplier
		data["applied_peak_multiplier"] = *response.AppliedPeakMultiplier
		data["timezone"] = *response.Timezone
	}
	appliedPeak, ok := upstreamBillingPeakMultiplierAt(data, observedAt)
	if !ok {
		return nil, fmt.Errorf("invalid peak billing response")
	}
	if response.PeakRateEnabled != nil && *response.PeakRateEnabled {
		if !equalBillingMultiplier(*response.AppliedPeakMultiplier, appliedPeak) {
			return nil, fmt.Errorf("inconsistent applied peak multiplier")
		}
	} else if response.AppliedPeakMultiplier != nil && !equalBillingMultiplier(*response.AppliedPeakMultiplier, 1) {
		return nil, fmt.Errorf("inconsistent applied peak multiplier")
	}
	if !equalBillingMultiplier(*response.EffectiveRateMultiplier, *response.ResolvedRateMultiplier*appliedPeak) {
		return nil, fmt.Errorf("inconsistent effective billing multiplier")
	}
	return data, nil
}

func upstreamBillingRateAt(data map[string]any, now time.Time) (float64, bool) {
	if scope, _ := data["billing_scope"].(string); scope != "token" {
		return 0, false
	}
	base, ok := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
	if !ok || base < 0 || math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, false
	}
	appliedPeak, ok := upstreamBillingPeakMultiplierAt(data, now)
	if !ok {
		return 0, false
	}
	base *= appliedPeak
	if math.IsNaN(base) || math.IsInf(base, 0) {
		return 0, false
	}
	return base, true
}

func upstreamBillingPeakMultiplierAt(data map[string]any, now time.Time) (float64, bool) {
	peakEnabled, ok := data["peak_rate_enabled"].(bool)
	if !ok {
		return 0, false
	}
	if !peakEnabled {
		return 1, true
	}

	start, startOK := data["peak_start"].(string)
	end, endOK := data["peak_end"].(string)
	timezoneName, timezoneOK := data["timezone"].(string)
	peakMultiplier, multiplierOK := resolveAccountExtraNumber(data, "peak_rate_multiplier")
	startMinute, validStart := parseMinutes(start)
	endMinute, validEnd := parseMinutes(end)
	if !startOK || !endOK || !timezoneOK || !multiplierOK || !validStart || !validEnd ||
		startMinute >= endMinute || peakMultiplier < 0 || math.IsNaN(peakMultiplier) || math.IsInf(peakMultiplier, 0) {
		return 0, false
	}
	location, err := time.LoadLocation(timezoneName)
	if err != nil {
		return 0, false
	}

	local := now.In(location)
	minute := local.Hour()*60 + local.Minute()
	if minute >= startMinute && minute < endMinute {
		return peakMultiplier, true
	}
	return 1, true
}

func equalBillingMultiplier(left, right float64) bool {
	if math.IsNaN(left) || math.IsNaN(right) || math.IsInf(left, 0) || math.IsInf(right, 0) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
	return math.Abs(left-right) <= 1e-9*scale
}

func decodeUpstreamBillingProbeSnapshot(extra map[string]any) *UpstreamBillingProbeSnapshot {
	if extra == nil {
		return nil
	}
	value, ok := extra[UpstreamBillingProbeExtraKey]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var snapshot UpstreamBillingProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Status == "" {
		return nil
	}
	if snapshot.Status != UpstreamBillingProbeStatusOK &&
		snapshot.Status != UpstreamBillingProbeStatusUnsupported &&
		snapshot.Status != UpstreamBillingProbeStatusFailed {
		return nil
	}
	return &snapshot
}

func isUpstreamBillingProbeAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey
}

func upstreamBillingProbeEnabled(account *Account) bool {
	if account == nil {
		return false
	}
	if account.Extra != nil {
		if enabled, ok := account.Extra[UpstreamBillingProbeEnabledExtraKey].(bool); ok {
			return enabled
		}
	}
	return false
}

func poolAutoPriorityEnabled(account *Account) bool {
	if account == nil || !account.IsPoolMode() {
		return false
	}
	if account.Extra != nil {
		if enabled, ok := account.Extra[PoolAutoPriorityEnabledExtraKey].(bool); ok {
			return enabled
		}
	}
	return true
}

func scheduledProbeModes(account *Account, billingGloballyEnabled, poolPriorityGloballyEnabled bool) (bool, bool) {
	if !isUpstreamBillingProbeAccount(account) {
		return false, false
	}
	return billingGloballyEnabled && upstreamBillingProbeEnabled(account),
		poolPriorityGloballyEnabled && poolAutoPriorityEnabled(account)
}

func effectiveProbeIntervalMinutes(includeBilling, includeModel bool, billingInterval, modelInterval int) int {
	interval := 0
	if includeBilling {
		interval = billingInterval
	}
	if includeModel && (interval == 0 || modelInterval < interval) {
		interval = modelInterval
	}
	if interval <= 0 {
		interval = upstreamBillingProbeDefaultIntervalMinutes
	}
	return interval
}

func (s *UpstreamBillingProbeService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func nextProbeDelay(intervalMinutes int, retryAfterDuration time.Duration) time.Duration {
	interval := time.Duration(intervalMinutes) * time.Minute
	if interval < upstreamBillingProbeMinIntervalMinutes*time.Minute {
		interval = upstreamBillingProbeMinIntervalMinutes * time.Minute
	}
	if interval > upstreamBillingProbeMaxDelay {
		interval = upstreamBillingProbeMaxDelay
	}
	jitterRange := interval / 5
	if jitterRange > 5*time.Minute {
		jitterRange = 5 * time.Minute
	}
	if jitterRange > 0 {
		interval += time.Duration(rand.Int64N(int64(jitterRange)*2+1)) - jitterRange
	}
	if retryAfterDuration > interval {
		// Retry-After is an explicit upstream instruction; do not shorten it
		// with the local maximum delay.
		return retryAfterDuration
	}
	if interval > upstreamBillingProbeMaxDelay {
		return upstreamBillingProbeMaxDelay
	}
	return interval
}

func nextProbeFailureDelay(intervalMinutes int, failureCount int, retryAfterDuration time.Duration) time.Duration {
	var delay time.Duration
	switch failureCount {
	case 1:
		delay = time.Minute
	case 2:
		delay = 2 * time.Minute
	default:
		delay = nextProbeDelay(intervalMinutes, 0)
	}
	if retryAfterDuration > delay {
		return retryAfterDuration
	}
	return delay
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func probeTimePtr(value time.Time) *time.Time {
	return &value
}

func probeLatencyMS(start time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 1
	}
	if elapsed < time.Millisecond {
		return 1
	}
	return elapsed.Milliseconds()
}

func safeProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrUpstreamBillingProbeAccountInvalid) {
		return ErrUpstreamBillingProbeAccountInvalid.Error()
	}
	if errors.Is(err, ErrUpstreamBillingProbeUnavailable) {
		return ErrUpstreamBillingProbeUnavailable.Error()
	}
	return "probe_failed"
}
