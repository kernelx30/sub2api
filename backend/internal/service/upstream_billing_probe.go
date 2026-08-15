package service

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
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
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	// These values live in accounts.extra so PR2 does not require a schema migration.
	UpstreamBillingProbeExtraKey           = "upstream_billing_probe"
	UpstreamBillingProbeEnabledExtraKey    = "upstream_billing_probe_enabled"
	PoolAutoPriorityEnabledExtraKey        = "pool_auto_priority_enabled"
	UpstreamBillingRateSyncEnabledExtraKey = "upstream_billing_rate_sync_enabled"

	upstreamBillingProbeDefaultIntervalMinutes = 5
	upstreamBillingProbeMinIntervalMinutes     = 5
	upstreamBillingProbeMaxIntervalMinutes     = 24 * 60
	upstreamBillingProbeCycleInterval          = time.Minute
	upstreamBillingProbeRequestTimeout         = 10 * time.Second
	upstreamBalanceProbeRequestTimeout         = 10 * time.Second
	upstreamBillingProbeMaxBodyBytes           = 64 * 1024
	upstreamBillingProbeMaxPerCycle            = 20
	upstreamBillingProbeConcurrency            = 4
	upstreamBillingProbeMaxDelay               = 24 * time.Hour
	// unsupported 账号的重探间隔倍数：上游不是 sub2api 中转就不会突然长出
	// /v1/sub2api/billing，按常规 interval 重排只会持续占满每周期
	// upstreamBillingProbeMaxPerCycle 个名额。
	upstreamBillingProbeUnsupportedDelayFactor = 8
	upstreamBillingProbeAccountRateScale       = 10000.0
	upstreamBillingProbeLeaderLockKey          = "upstream:billing:probe:leader"
	upstreamBillingProbeLeaderLockTTL          = 2 * time.Minute
	poolAutoPriorityDefaultIntervalMinutes     = 5
	poolAutoPriorityMinIntervalMinutes         = 5
	poolAutoPriorityMaxIntervalMinutes         = 60
	upstreamModelProbeHistoryLimit             = 12
	upstreamModelProbeMetricsWindow            = time.Hour
)

// UpstreamBillingProbeMaxBatchSize limits one manual batch and one runner cycle.
const UpstreamBillingProbeMaxBatchSize = upstreamBillingProbeMaxPerCycle

// upstreamBillingRateSyncMaxMultiplier bounds the value the automatic
// write-back may push into accounts.rate_multiplier.
//
// No other code path bounds that column from above — admins may type any
// non-negative number and the only ceiling is the DECIMAL(10,4) column itself
// (999999.9999). That ceiling is meaningless as a guard: rate_multiplier
// scales the per-request account cost that feeds quota_used, so a single
// declared 999999 would exhaust any account quota on the first request and
// poison cost reporting. 100 is picked as a deliberately generous bound: it is
// two orders of magnitude above the 1.0 default and far above any plausible
// upstream resale markup, so no legitimate declaration is rejected while an
// absurd or hostile one cannot reach the quota control plane unattended.
// It only constrains the automatic path; manual edits keep their old range.
const upstreamBillingRateSyncMaxMultiplier = 100.0

var (
	ErrUpstreamBillingProbeUnavailable = infraerrors.ServiceUnavailable(
		"UPSTREAM_BILLING_PROBE_UNAVAILABLE", "upstream billing probe is unavailable",
	)
	ErrUpstreamBillingProbeAccountInvalid = infraerrors.BadRequest(
		"UPSTREAM_BILLING_PROBE_ACCOUNT_INVALID", "account is not an API key account",
	)
	ErrUpstreamBillingProbeIdentityChanged = infraerrors.Conflict(
		"UPSTREAM_BILLING_PROBE_IDENTITY_CHANGED", "account identity changed during upstream billing probe; retry the probe",
	)
	ErrUpstreamBillingRateSyncBulkConflict = infraerrors.Conflict(
		"UPSTREAM_BILLING_RATE_SYNC_BULK_CONFLICT",
		"account rate multiplier cannot be changed in bulk while upstream billing rate sync is enabled",
	)
	ErrUpstreamBillingRateSyncConflict = infraerrors.Conflict(
		"UPSTREAM_BILLING_RATE_SYNC_CONFLICT",
		"account rate multiplier cannot be changed while upstream billing rate sync is enabled",
	)
)

const (
	UpstreamBillingProbeStatusOK          = "ok"
	UpstreamBillingProbeStatusUnsupported = "unsupported"
	UpstreamBillingProbeStatusFailed      = "failed"

	UpstreamBalanceSourceAPIKeyQuota  = "api_key_quota"
	UpstreamBalanceSourceWallet       = "wallet_balance"
	UpstreamBalanceSourceSubscription = "subscription_quota"
)

const (
	upstreamBalanceObservedAtKey = "available_balance_observed_at"
	upstreamBalanceFreshUntilKey = "available_balance_fresh_until"
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

// UpstreamModelProbeSample keeps the bounded evidence used by pool auto-priority.
// ErrorType contains a sanitized internal category rather than an upstream body.
type UpstreamModelProbeSample struct {
	Status      string    `json:"status"`
	LatencyMS   int64     `json:"latency_ms,omitempty"`
	HTTPStatus  int       `json:"http_status,omitempty"`
	ErrorType   string    `json:"error_type,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
}

// UpstreamBillingProbeSnapshot is persisted in accounts.extra. Data is kept as
// a sanitized map so future response fields do not require a database change.
type UpstreamBillingProbeSnapshot struct {
	Status                  string                     `json:"status"`
	BillingProbeAttempted   bool                       `json:"billing_probe_attempted,omitempty"`
	Data                    map[string]any             `json:"data,omitempty"`
	ReceivedAt              *time.Time                 `json:"received_at,omitempty"`
	FreshUntil              *time.Time                 `json:"fresh_until,omitempty"`
	LastAttemptAt           time.Time                  `json:"last_attempt_at"`
	NextProbeAt             time.Time                  `json:"next_probe_at"`
	FailureCount            int                        `json:"failure_count,omitempty"`
	HTTPStatus              int                        `json:"http_status,omitempty"`
	LatencyMS               int64                      `json:"latency_ms,omitempty"`
	ModelProbeStatus        string                     `json:"model_probe_status,omitempty"`
	ModelProbeModel         string                     `json:"model_probe_model,omitempty"`
	ModelProbeEndpoint      string                     `json:"model_probe_endpoint,omitempty"`
	ModelProbeLatencyMS     int64                      `json:"model_probe_latency_ms,omitempty"`
	ModelProbeHTTPStatus    int                        `json:"model_probe_http_status,omitempty"`
	ModelProbeLastError     string                     `json:"model_probe_last_error,omitempty"`
	ModelProbeLastAttemptAt *time.Time                 `json:"model_probe_last_attempt_at,omitempty"`
	ModelProbeFreshUntil    *time.Time                 `json:"model_probe_fresh_until,omitempty"`
	ModelProbeNextAt        *time.Time                 `json:"model_probe_next_at,omitempty"`
	ModelProbeFailureCount  int                        `json:"model_probe_failure_count,omitempty"`
	ModelProbeHistory       []UpstreamModelProbeSample `json:"model_probe_history,omitempty"`
	ModelProbeSampleCount   int                        `json:"model_probe_sample_count,omitempty"`
	ModelProbeSuccessCount  int                        `json:"model_probe_success_count,omitempty"`
	ModelProbeSuccessRate   float64                    `json:"model_probe_success_rate,omitempty"`
	ModelProbeP50LatencyMS  int64                      `json:"model_probe_p50_latency_ms,omitempty"`
	ModelProbeP95LatencyMS  int64                      `json:"model_probe_p95_latency_ms,omitempty"`
	ModelProbeConsecutiveOK int                        `json:"model_probe_consecutive_ok,omitempty"`
	ModelProbeConsecutiveNG int                        `json:"model_probe_consecutive_ng,omitempty"`
	LastError               string                     `json:"last_error,omitempty"`
	// SyncedRateMultiplier records the value this probe wrote into
	// accounts.rate_multiplier. It is only set when the account opted into rate
	// sync and the declared value passed the write-back range check, so the
	// stored snapshot always answers "did this probe move the account rate, and
	// to what" without a separate history table.
	SyncedRateMultiplier *float64 `json:"synced_rate_multiplier,omitempty"`
}

// UpstreamBillingProbeResult is returned by manual probe endpoints.
type UpstreamBillingProbeResult struct {
	AccountID int64                         `json:"account_id"`
	Snapshot  *UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
	Error     string                        `json:"error,omitempty"`
}

type upstreamBillingProbeResponse struct {
	Object                    string   `json:"object"`
	SchemaVersion             int      `json:"schema_version"`
	BillingScope              string   `json:"billing_scope"`
	AvailableBalance          *float64 `json:"available_balance"`
	AvailableBalanceSource    *string  `json:"available_balance_source"`
	AvailableBalanceUnlimited *bool    `json:"available_balance_unlimited"`
	GroupRateMultiplier       *float64 `json:"group_rate_multiplier"`
	UserRateMultiplier        *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier    *float64 `json:"resolved_rate_multiplier"`
	PeakRateEnabled           *bool    `json:"peak_rate_enabled"`
	PeakStart                 *string  `json:"peak_start"`
	PeakEnd                   *string  `json:"peak_end"`
	PeakRateMultiplier        *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier     *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier   *float64 `json:"effective_rate_multiplier"`
	Timezone                  *string  `json:"timezone"`
	ObservedAt                string   `json:"observed_at"`
}

type upstreamUsageBalanceResponse struct {
	Mode      string   `json:"mode"`
	IsValid   *bool    `json:"isValid"`
	Remaining *float64 `json:"remaining"`
	Balance   *float64 `json:"balance"`
	Unit      string   `json:"unit"`
	Quota     *struct {
		Remaining *float64 `json:"remaining"`
		Unit      string   `json:"unit"`
	} `json:"quota"`
	Subscription json.RawMessage `json:"subscription"`
}

type upstreamAvailableBalanceProbeResult struct {
	AvailableBalance *float64
	Unlimited        bool
	Source           string
	ObservedAt       time.Time
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
	UpdateUpstreamBillingProbeSnapshot(context.Context, *Account, *UpstreamBillingProbeSnapshot, *float64) error
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
	// Non-production repositories and older adapters do not expose the optimized
	// due-query interface. Load every supported API-key platform and let RunDue
	// apply the same explicit-switch-or-pool-mode eligibility rule.
	platforms := []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok}
	accounts := make([]Account, 0)
	seen := make(map[int64]struct{})
	for _, platform := range platforms {
		platformAccounts, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			return nil, err
		}
		for i := range platformAccounts {
			if _, ok := seen[platformAccounts[i].ID]; ok {
				continue
			}
			seen[platformAccounts[i].ID] = struct{}{}
			accounts = append(accounts, platformAccounts[i])
		}
	}
	return accounts, nil
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
	updates := map[string]any{UpstreamBillingProbeEnabledExtraKey: enabled}
	if !enabled {
		updates[UpstreamBillingRateSyncEnabledExtraKey] = false
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, updates)
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

type upstreamModelProbeMetrics struct {
	SampleCount          int
	SuccessCount         int
	SuccessRate          float64
	P50LatencyMS         int64
	P95LatencyMS         int64
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
}

func (s *UpstreamBillingProbeService) probeLoadedAccount(ctx context.Context, account *Account, intervalMinutes int, includeBilling, includeModel bool) (*UpstreamBillingProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "transport_unavailable", includeBilling, includeModel)
	}
	// 平台放宽后取数直读 credentials：所有 API-key 平台的密钥与自定义上游
	// 统一存放在 credentials.api_key / credentials.base_url。
	apiKey := account.GetCredential("api_key")
	if apiKey == "" {
		return s.persistPlannedProbeFailure(ctx, account, intervalMinutes, now, "missing_api_key", includeBilling, includeModel)
	}
	baseURL := account.GetCredential("base_url")
	if account.Platform == PlatformOpenAI {
		if baseURL == "" {
			// 保持官方语义：OpenAI 账号无自定义 base 时探官方域（404 → unsupported）。
			baseURL = "https://api.openai.com"
		}
	} else if upstreamBillingProbeTargetIsOfficialAPI(baseURL) {
		// 其他平台 base_url 为空或指向官方 API 根域（前端创建时会把空值
		// 填成官方默认域，且提供 us-east-1.api.x.ai 等官方区域预设）⇒
		// 必无 /v1/sub2api/billing；不发请求，直接记 unsupported，避免
		// 拿账号 Key 周期性请求官方域的不存在路径。
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "unsupported", 0, nil)
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
	profile := HTTPUpstreamProfileDefault
	if account.Platform == PlatformOpenAI {
		profile = HTTPUpstreamProfileOpenAI
	}
	probeAvailableBalance := func() *upstreamAvailableBalanceProbeResult {
		return s.probeUpstreamAvailableBalance(ctx, now, account, normalizedBaseURL, apiKey, proxyURL, tlsProfile, profile)
	}

	// Pool-mode routing needs real model reachability. Ordinary channel-status
	// probes keep the original billing-only behavior to avoid unnecessary spend.
	var modelProbe *upstreamModelProbeResult
	if includeModel {
		modelProbe = s.probeUpstreamModel(ctx, account, normalizedBaseURL, apiKey, proxyURL, tlsProfile)
	}
	if !includeBilling {
		return s.persistModelOnlyProbe(ctx, account, intervalMinutes, now, modelProbe, probeAvailableBalance())
	}

	probeURL := buildOpenAIEndpointURL(normalizedBaseURL, "/v1/sub2api/billing")
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBillingProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, bytes.NewReader(nil))
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "request_build_failed", 0, modelProbe)
	}
	// OpenAI 账号保持官方 openai 传输画像；其他平台探测走默认画像。
	reqCtx := WithHTTPUpstreamProfile(req.Context(), profile)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	probeStarted := time.Now()
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "request_failed", 0, modelProbe, probeAvailableBalance())
	}
	if resp == nil || resp.Body == nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "empty_response", 0, modelProbe, probeAvailableBalance())
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamBillingProbeMaxBodyBytes+1))
	latencyMS := probeLatencyMS(probeStarted)
	if readErr != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_read_failed", retryAfter(resp.Header, now), modelProbe, probeAvailableBalance())
	}
	if len(body) > upstreamBillingProbeMaxBodyBytes {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "response_too_large", retryAfter(resp.Header, now), modelProbe, probeAvailableBalance())
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "unsupported", retryAfter(resp.Header, now), modelProbe, probeAvailableBalance())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "http_error", retryAfter(resp.Header, now), modelProbe, probeAvailableBalance())
	}
	data, err := parseUpstreamBillingProbeResponse(body)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, resp.StatusCode, "invalid_response", retryAfter(resp.Header, now), modelProbe, probeAvailableBalance())
	}
	if shouldProbeUpstreamUsageBalance(data) {
		data = mergeUpstreamAvailableBalance(data, probeAvailableBalance(), now, intervalMinutes)
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
	// Account-level range and precision only matter when a write-back is
	// requested. A successful discovery remains successful even when an
	// unusable declaration is rejected for automatic synchronization.
	var syncRate *float64
	previousRate := account.BillingRateMultiplier()
	if upstreamBillingRateSyncEnabled(account) {
		if value, valid := upstreamBillingProbeSyncRate(data); valid {
			syncRate = &value
			snapshot.SyncedRateMultiplier = &value
		} else {
			declared, _ := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
			slog.Warn("upstream_billing_rate_sync_rejected",
				"source", "upstream_billing_probe",
				"account_id", account.ID,
				"declared_resolved_rate_multiplier", declared,
				"max_rate_multiplier", upstreamBillingRateSyncMaxMultiplier,
				"current_rate_multiplier", previousRate,
			)
		}
	}
	if err := s.updateSnapshot(ctx, account, snapshot, syncRate); err != nil {
		return nil, err
	}
	if syncRate != nil {
		// The background write-back uses repository SQL instead of the admin
		// route, so record the otherwise unaudited change in structured logs.
		slog.Info("upstream_billing_rate_sync_applied",
			"source", "upstream_billing_probe",
			"account_id", account.ID,
			"old_rate_multiplier", previousRate,
			"new_rate_multiplier", *syncRate,
		)
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) probeUpstreamAvailableBalance(
	ctx context.Context,
	now time.Time,
	account *Account,
	normalizedBaseURL string,
	apiKey string,
	proxyURL string,
	tlsProfile *tlsfingerprint.Profile,
	profile HTTPUpstreamProfile,
) *upstreamAvailableBalanceProbeResult {
	probeURL := buildOpenAIEndpointURL(normalizedBaseURL, "/v1/usage")
	probeCtx, cancel := context.WithTimeout(ctx, upstreamBalanceProbeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, bytes.NewReader(nil))
	if err != nil {
		return nil
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), profile)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil || resp == nil || resp.Body == nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBillingProbeMaxBodyBytes+1))
	if err != nil || len(body) > upstreamBillingProbeMaxBodyBytes {
		return nil
	}
	result, err := parseUpstreamUsageBalanceResponse(body)
	if err != nil {
		return nil
	}
	result.ObservedAt = now
	return result
}

func parseUpstreamUsageBalanceResponse(body []byte) (*upstreamAvailableBalanceProbeResult, error) {
	var response upstreamUsageBalanceResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.IsValid != nil && !*response.IsValid {
		return nil, fmt.Errorf("upstream usage response reports invalid API key")
	}
	validMoney := func(value *float64) bool {
		return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
	}
	validUSD := func(unit string) bool {
		return strings.EqualFold(strings.TrimSpace(unit), "USD")
	}

	switch strings.TrimSpace(response.Mode) {
	case "quota_limited":
		remaining := response.Remaining
		unit := response.Unit
		if response.Quota != nil {
			if response.Quota.Remaining != nil {
				remaining = response.Quota.Remaining
			}
			if strings.TrimSpace(response.Quota.Unit) != "" {
				unit = response.Quota.Unit
			}
		}
		if !validMoney(remaining) || !validUSD(unit) || *remaining < 0 {
			return nil, fmt.Errorf("invalid API key quota balance")
		}
		value := *remaining
		return &upstreamAvailableBalanceProbeResult{
			AvailableBalance: &value,
			Source:           UpstreamBalanceSourceAPIKeyQuota,
		}, nil
	case "unrestricted":
		if validMoney(response.Balance) && validUSD(response.Unit) {
			value := *response.Balance
			return &upstreamAvailableBalanceProbeResult{
				AvailableBalance: &value,
				Source:           UpstreamBalanceSourceWallet,
			}, nil
		}
		if validMoney(response.Remaining) && validUSD(response.Unit) && len(bytes.TrimSpace(response.Subscription)) > 0 && string(bytes.TrimSpace(response.Subscription)) != "null" {
			if *response.Remaining < 0 {
				return &upstreamAvailableBalanceProbeResult{
					Unlimited: true,
					Source:    UpstreamBalanceSourceSubscription,
				}, nil
			}
			value := *response.Remaining
			return &upstreamAvailableBalanceProbeResult{
				AvailableBalance: &value,
				Source:           UpstreamBalanceSourceSubscription,
			}, nil
		}
	}

	return nil, fmt.Errorf("upstream usage response has no USD balance")
}

func shouldProbeUpstreamUsageBalance(data map[string]any) bool {
	if data == nil {
		return true
	}
	source, _ := data["available_balance_source"].(string)
	switch strings.TrimSpace(source) {
	case UpstreamBalanceSourceAPIKeyQuota:
		if unlimited, ok := data["available_balance_unlimited"].(bool); ok && unlimited {
			return true
		}
		balance, ok := resolveAccountExtraNumber(data, "available_balance")
		return !ok || balance < 0 || math.IsNaN(balance) || math.IsInf(balance, 0)
	default:
		return true
	}
}

func mergeUpstreamAvailableBalance(
	data map[string]any,
	result *upstreamAvailableBalanceProbeResult,
	now time.Time,
	intervalMinutes int,
) map[string]any {
	if result == nil {
		return data
	}
	merged := mergeMap(nil, data)
	delete(merged, "available_balance")
	delete(merged, "available_balance_unlimited")
	merged["available_balance_source"] = result.Source
	if result.AvailableBalance != nil {
		merged["available_balance"] = *result.AvailableBalance
	}
	if result.Unlimited {
		merged["available_balance_unlimited"] = true
	}
	observedAt := result.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	merged[upstreamBalanceObservedAtKey] = observedAt.UTC().Format(time.RFC3339Nano)
	merged[upstreamBalanceFreshUntilKey] = observedAt.Add(2 * time.Duration(intervalMinutes) * time.Minute).UTC().Format(time.RFC3339Nano)
	return merged
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
	probePrompt := upstreamModelProbePrompt(account.ID, time.Now())
	var payload []byte
	if useResponses {
		result.Endpoint = "responses"
		payload, _ = json.Marshal(createOpenAIResponsesModelProbePayload(result.Model, probePrompt))
	} else {
		result.Endpoint = "chat_completions"
		payload, _ = json.Marshal(createOpenAIChatCompletionsTestPayload(result.Model, probePrompt))
	}

	probeURL := buildOpenAIResponsesURL(normalizedBaseURL)
	if !useResponses {
		probeURL = buildOpenAIChatCompletionsURL(normalizedBaseURL)
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
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")
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
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		result.LatencyMS = probeLatencyMS(started)
		result.Status = UpstreamBillingProbeStatusUnsupported
		result.LastError = "unsupported"
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.LatencyMS = probeLatencyMS(started)
		result.LastError = "http_error"
		return result
	}
	if readError := readUpstreamModelProbeFirstOutput(resp.Body, useResponses); readError != "" {
		result.LatencyMS = probeLatencyMS(started)
		result.LastError = readError
		return result
	}
	result.LatencyMS = probeLatencyMS(started)
	result.Status = UpstreamBillingProbeStatusOK
	result.LastError = ""
	return result
}

func upstreamModelProbePrompt(accountID int64, now time.Time) string {
	return fmt.Sprintf("Reply with OK only. Probe nonce: %d-%d", accountID, now.UTC().UnixNano())
}

func createOpenAIResponsesModelProbePayload(model, prompt string) map[string]any {
	payload := createOpenAITestPayload(model, false)
	payload["input"] = []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": prompt,
				},
			},
		},
	}
	// Real Codex traffic uses store=false. Keeping the probe on the same path
	// prevents a repeated static health check from measuring an upstream cache.
	payload["store"] = false
	return payload
}

// readUpstreamModelProbeFirstOutput measures the same user-visible boundary as
// gateway TTFT: preamble frames do not count, and the probe returns as soon as
// the first text/tool delta arrives instead of waiting for the whole response.
func readUpstreamModelProbeFirstOutput(body io.Reader, useResponses bool) string {
	if body == nil {
		return "empty_response"
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), responsesProbeMaxBodyBytes)
	eventType := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			eventType = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		payload := line
		if strings.HasPrefix(payload, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return "empty_response"
		}
		if !gjson.Valid(payload) {
			continue
		}

		payloadBytes := []byte(payload)
		payloadType := strings.TrimSpace(gjson.GetBytes(payloadBytes, "type").String())
		if payloadType == "" {
			payloadType = eventType
		}
		if payloadType == "error" || payloadType == "response.failed" || gjson.GetBytes(payloadBytes, "error").Exists() {
			return "stream_error"
		}
		if upstreamModelProbePayloadHasVisibleOutput(payloadBytes, payloadType, useResponses) {
			return ""
		}
		if payloadType == "response.completed" || payloadType == "response.done" {
			return "empty_response"
		}
		for _, choice := range gjson.GetBytes(payloadBytes, "choices").Array() {
			if strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
				return "empty_response"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return "response_too_large"
		}
		return "response_read_failed"
	}
	return "empty_response"
}

func upstreamModelProbePayloadHasVisibleOutput(payload []byte, eventType string, useResponses bool) bool {
	if useResponses {
		if openAIStreamDataStartsVisibleOutput(string(payload), eventType) {
			return true
		}
		if strings.TrimSpace(gjson.GetBytes(payload, "output_text").String()) != "" {
			return true
		}
		for _, item := range gjson.GetBytes(payload, "output").Array() {
			if openAIStreamItemHasVisibleOutput(item) {
				return true
			}
		}
		return false
	}

	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		for _, path := range []string{
			"delta.content",
			"delta.reasoning_content",
			"delta.reasoning",
			"message.content",
		} {
			if strings.TrimSpace(choice.Get(path).String()) != "" {
				return true
			}
		}
	}
	return false
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
	if snapshot == nil {
		return
	}
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	inheritUpstreamModelProbeSnapshot(snapshot, previous)
	if result == nil {
		applyUpstreamModelProbeMetrics(snapshot, now)
		return
	}
	appendUpstreamModelProbeSample(snapshot, result, now)
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
	applyUpstreamModelProbeMetrics(snapshot, now)
}

func inheritUpstreamModelProbeSnapshot(snapshot, previous *UpstreamBillingProbeSnapshot) {
	if snapshot == nil || previous == nil || snapshot.ModelProbeStatus != "" ||
		snapshot.ModelProbeLastAttemptAt != nil || len(snapshot.ModelProbeHistory) > 0 {
		return
	}
	snapshot.ModelProbeStatus = previous.ModelProbeStatus
	snapshot.ModelProbeModel = previous.ModelProbeModel
	snapshot.ModelProbeEndpoint = previous.ModelProbeEndpoint
	snapshot.ModelProbeLatencyMS = previous.ModelProbeLatencyMS
	snapshot.ModelProbeHTTPStatus = previous.ModelProbeHTTPStatus
	snapshot.ModelProbeLastError = previous.ModelProbeLastError
	snapshot.ModelProbeLastAttemptAt = cloneProbeTimePtr(previous.ModelProbeLastAttemptAt)
	snapshot.ModelProbeFreshUntil = cloneProbeTimePtr(previous.ModelProbeFreshUntil)
	snapshot.ModelProbeNextAt = cloneProbeTimePtr(previous.ModelProbeNextAt)
	snapshot.ModelProbeFailureCount = previous.ModelProbeFailureCount
	snapshot.ModelProbeHistory = append([]UpstreamModelProbeSample(nil), previous.ModelProbeHistory...)
	snapshot.ModelProbeSampleCount = previous.ModelProbeSampleCount
	snapshot.ModelProbeSuccessCount = previous.ModelProbeSuccessCount
	snapshot.ModelProbeSuccessRate = previous.ModelProbeSuccessRate
	snapshot.ModelProbeP50LatencyMS = previous.ModelProbeP50LatencyMS
	snapshot.ModelProbeP95LatencyMS = previous.ModelProbeP95LatencyMS
	snapshot.ModelProbeConsecutiveOK = previous.ModelProbeConsecutiveOK
	snapshot.ModelProbeConsecutiveNG = previous.ModelProbeConsecutiveNG
}

func appendUpstreamModelProbeSample(snapshot *UpstreamBillingProbeSnapshot, result *upstreamModelProbeResult, now time.Time) {
	if snapshot == nil || result == nil {
		return
	}
	history := append([]UpstreamModelProbeSample(nil), snapshot.ModelProbeHistory...)
	if len(history) == 0 && snapshot.ModelProbeStatus != "" && snapshot.ModelProbeLastAttemptAt != nil {
		history = append(history, UpstreamModelProbeSample{
			Status:      snapshot.ModelProbeStatus,
			LatencyMS:   snapshot.ModelProbeLatencyMS,
			HTTPStatus:  snapshot.ModelProbeHTTPStatus,
			ErrorType:   snapshot.ModelProbeLastError,
			AttemptedAt: snapshot.ModelProbeLastAttemptAt.UTC(),
		})
	}
	history = append(history, UpstreamModelProbeSample{
		Status:      result.Status,
		LatencyMS:   result.LatencyMS,
		HTTPStatus:  result.HTTPStatus,
		ErrorType:   result.LastError,
		AttemptedAt: now.UTC(),
	})
	if len(history) > upstreamModelProbeHistoryLimit {
		history = append([]UpstreamModelProbeSample(nil), history[len(history)-upstreamModelProbeHistoryLimit:]...)
	}
	snapshot.ModelProbeHistory = history
}

func applyUpstreamModelProbeMetrics(snapshot *UpstreamBillingProbeSnapshot, now time.Time) {
	if snapshot == nil {
		return
	}
	metrics := upstreamModelProbeWindowMetrics(snapshot, now)
	snapshot.ModelProbeSampleCount = metrics.SampleCount
	snapshot.ModelProbeSuccessCount = metrics.SuccessCount
	snapshot.ModelProbeSuccessRate = metrics.SuccessRate
	snapshot.ModelProbeP50LatencyMS = metrics.P50LatencyMS
	snapshot.ModelProbeP95LatencyMS = metrics.P95LatencyMS
	snapshot.ModelProbeConsecutiveOK = metrics.ConsecutiveSuccesses
	snapshot.ModelProbeConsecutiveNG = metrics.ConsecutiveFailures
}

func upstreamModelProbeWindowMetrics(snapshot *UpstreamBillingProbeSnapshot, now time.Time) upstreamModelProbeMetrics {
	if snapshot == nil {
		return upstreamModelProbeMetrics{}
	}
	history := append([]UpstreamModelProbeSample(nil), snapshot.ModelProbeHistory...)
	if len(history) == 0 && snapshot.ModelProbeStatus != "" && snapshot.ModelProbeLastAttemptAt != nil {
		history = append(history, UpstreamModelProbeSample{
			Status:      snapshot.ModelProbeStatus,
			LatencyMS:   snapshot.ModelProbeLatencyMS,
			HTTPStatus:  snapshot.ModelProbeHTTPStatus,
			ErrorType:   snapshot.ModelProbeLastError,
			AttemptedAt: snapshot.ModelProbeLastAttemptAt.UTC(),
		})
	}
	windowStart := now.UTC().Add(-upstreamModelProbeMetricsWindow)
	windowEnd := now.UTC()
	recent := history[:0]
	for _, sample := range history {
		if sample.AttemptedAt.IsZero() || sample.AttemptedAt.Before(windowStart) || sample.AttemptedAt.After(windowEnd) {
			continue
		}
		if sample.Status != UpstreamBillingProbeStatusOK &&
			sample.Status != UpstreamBillingProbeStatusFailed &&
			sample.Status != UpstreamBillingProbeStatusUnsupported {
			continue
		}
		recent = append(recent, sample)
	}
	if len(recent) == 0 {
		return upstreamModelProbeMetrics{}
	}
	sort.SliceStable(recent, func(i, j int) bool {
		return recent[i].AttemptedAt.Before(recent[j].AttemptedAt)
	})

	metrics := upstreamModelProbeMetrics{SampleCount: len(recent)}
	latencies := make([]int64, 0, len(recent))
	for _, sample := range recent {
		if sample.Status != UpstreamBillingProbeStatusOK {
			continue
		}
		metrics.SuccessCount++
		if sample.LatencyMS > 0 {
			latencies = append(latencies, sample.LatencyMS)
		}
	}
	metrics.SuccessRate = float64(metrics.SuccessCount) / float64(metrics.SampleCount)
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Status == UpstreamBillingProbeStatusOK {
			if metrics.ConsecutiveFailures > 0 {
				break
			}
			metrics.ConsecutiveSuccesses++
			continue
		}
		if metrics.ConsecutiveSuccesses > 0 {
			break
		}
		metrics.ConsecutiveFailures++
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		metrics.P50LatencyMS = nearestRankLatency(latencies, 50)
		metrics.P95LatencyMS = nearestRankLatency(latencies, 95)
	}
	return metrics
}

func nearestRankLatency(sortedLatencies []int64, percentile int) int64 {
	if len(sortedLatencies) == 0 {
		return 0
	}
	if percentile <= 0 {
		return sortedLatencies[0]
	}
	if percentile >= 100 {
		return sortedLatencies[len(sortedLatencies)-1]
	}
	index := (percentile*len(sortedLatencies) + 99) / 100
	if index < 1 {
		index = 1
	}
	return sortedLatencies[index-1]
}

func cloneProbeTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
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
	balanceProbe ...*upstreamAvailableBalanceProbeResult,
) (*UpstreamBillingProbeSnapshot, error) {
	if modelProbe == nil {
		modelProbe = failedUpstreamModelProbe(account, "model_probe_unavailable")
	}
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	snapshot := &UpstreamBillingProbeSnapshot{}
	if previous != nil {
		*snapshot = *previous
	}
	hadPreviousData := snapshot.Data != nil
	if len(balanceProbe) > 0 {
		snapshot.Data = mergeUpstreamAvailableBalance(snapshot.Data, balanceProbe[0], now, intervalMinutes)
	}
	applyScheduledUpstreamModelProbe(snapshot, account, modelProbe, now, intervalMinutes, 0)
	snapshot.LastAttemptAt = now
	if snapshot.ModelProbeNextAt != nil {
		snapshot.NextProbeAt = *snapshot.ModelProbeNextAt
	}
	if snapshot.Status == "" {
		snapshot.Status = modelProbe.Status
	}
	if !hadPreviousData {
		snapshot.FailureCount = snapshot.ModelProbeFailureCount
		snapshot.LastError = modelProbe.LastError
	}
	if err := s.updateSnapshot(ctx, account, snapshot, nil); err != nil {
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
	balanceProbe ...*upstreamAvailableBalanceProbeResult,
) (*UpstreamBillingProbeSnapshot, error) {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	failureCount := nextProbeFailureCount(account)
	status := UpstreamBillingProbeStatusFailed
	delay := nextProbeFailureDelay(intervalMinutes, failureCount, retryAfterDuration)
	if reason == "unsupported" {
		status = UpstreamBillingProbeStatusUnsupported
		delay = unsupportedProbeDelay(intervalMinutes, retryAfterDuration)
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:                status,
		BillingProbeAttempted: true,
		LastAttemptAt:         now,
		NextProbeAt:           now.Add(delay),
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
	if len(balanceProbe) > 0 {
		snapshot.Data = mergeUpstreamAvailableBalance(snapshot.Data, balanceProbe[0], now, intervalMinutes)
	}
	applyScheduledUpstreamModelProbe(snapshot, account, modelProbe, now, intervalMinutes, retryAfterDuration)
	if snapshot.ModelProbeNextAt != nil && snapshot.ModelProbeNextAt.Before(snapshot.NextProbeAt) {
		snapshot.NextProbeAt = *snapshot.ModelProbeNextAt
	}
	if err := s.updateSnapshot(ctx, account, snapshot, nil); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) updateSnapshot(
	ctx context.Context,
	account *Account,
	snapshot *UpstreamBillingProbeSnapshot,
	rateMultiplier *float64,
) error {
	writer, ok := s.accountRepo.(upstreamBillingProbeSnapshotWriter)
	if !ok {
		return ErrUpstreamBillingProbeUnavailable
	}
	return writer.UpdateUpstreamBillingProbeSnapshot(ctx, account, snapshot, rateMultiplier)
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
	if response.AvailableBalanceSource != nil {
		source := strings.TrimSpace(*response.AvailableBalanceSource)
		switch source {
		case UpstreamBalanceSourceAPIKeyQuota, UpstreamBalanceSourceWallet, UpstreamBalanceSourceSubscription:
		default:
			return nil, fmt.Errorf("invalid available balance source")
		}
		if response.AvailableBalance != nil {
			if math.IsNaN(*response.AvailableBalance) || math.IsInf(*response.AvailableBalance, 0) ||
				(source != UpstreamBalanceSourceWallet && *response.AvailableBalance < 0) {
				return nil, fmt.Errorf("invalid available balance")
			}
			data["available_balance"] = *response.AvailableBalance
		}
		if response.AvailableBalanceUnlimited != nil {
			data["available_balance_unlimited"] = *response.AvailableBalanceUnlimited
		}
		if response.AvailableBalance != nil || response.AvailableBalanceUnlimited != nil {
			data["available_balance_source"] = source
		}
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

// upstreamBillingProbeSyncRate converts the declared multiplier into the value
// the automatic write-back may store in accounts.rate_multiplier, at the
// precision that column supports (DECIMAL(10,4)).
//
// It reads resolved_rate_multiplier, not effective_rate_multiplier: the
// effective value folds in the peak coefficient that happened to apply at the
// instant of the probe, so writing it would freeze one probe cycle's peak (or
// off-peak) factor into a static column, while display and scheduling
// recompute the peak factor for the current time through upstreamBillingRateAt.
//
// The accepted range is deliberately narrower than the column:
//   - 0 is rejected. accountCost multiplies the request cost by this value, so
//     an upstream-declared 0 would stop quota_used from ever growing and every
//     admin-configured account quota and cost alert would silently stop
//     working. Admins may still set 0 by hand; only the automatic path refuses.
//   - anything above upstreamBillingRateSyncMaxMultiplier is rejected.
//
// A rejected declaration leaves the current multiplier untouched; the probe
// still records an OK snapshot carrying the raw declaration for display.
func upstreamBillingProbeSyncRate(data map[string]any) (float64, bool) {
	value, ok := resolveAccountExtraNumber(data, "resolved_rate_multiplier")
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	rounded := math.Round(value*upstreamBillingProbeAccountRateScale) / upstreamBillingProbeAccountRateScale
	if rounded <= 0 || rounded > upstreamBillingRateSyncMaxMultiplier {
		return 0, false
	}
	return rounded, true
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

// IsUpstreamBillingProbeIdentity reports whether an account identity may opt
// in to the upstream billing probe. `/v1/sub2api/billing` is a key-scoped
// sub2api convention shared by the five supported API-key platforms.
// Non-sub2api upstreams return 404 and the snapshot records "unsupported".
// Only AccountTypeAPIKey is in scope. OAuth/Bedrock hold no static API key to
// present at all; AccountTypeUpstream (antigravity relay accounts) does carry
// a base_url plus a static api_key, but it is deliberately left out of the
// current supported set. New antigravity relay accounts are created with
// type=apikey by the admin form, so only pre-existing type=upstream rows
// cannot turn the probe on.
func IsUpstreamBillingProbeIdentity(platform, accountType string) bool {
	if accountType != AccountTypeAPIKey {
		return false
	}
	switch platform {
	case PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok:
		return true
	default:
		return false
	}
}

func isUpstreamBillingProbeAccount(account *Account) bool {
	return account != nil && IsUpstreamBillingProbeIdentity(account.Platform, account.Type)
}

// upstreamBillingProbeOfficialAPIDomains lists the root domains of official
// provider APIs. The create form fills empty base_url values with official
// defaults (and offers official regional presets like us-east-1.api.x.ai),
// so probing them would send the account key to an official API path that
// cannot exist. Matching is by registrable root domain — exact host or any
// subdomain, after stripping the port and a trailing DNS dot — because no
// third-party sub2api relay can live under these domains, while custom
// relays (the only targets that can answer /v1/sub2api/billing) always do
// probe. OpenAI-platform accounts never reach this check: they keep the
// upstream-official behavior of probing api.openai.com.
// ollama.com is a first-class configuration here (Ollama Cloud accounts are
// platform openai/anthropic with base_url https://ollama.com/v1), and it is
// an official provider API just like the rest, so it belongs on this list.
var upstreamBillingProbeOfficialAPIDomains = []string{
	"anthropic.com",
	"googleapis.com",
	"x.ai",
	"grok.com",
	"openai.com",
	"ollama.com",
}

func upstreamBillingProbeTargetIsOfficialAPI(baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return true
	}
	for _, domain := range upstreamBillingProbeOfficialAPIDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
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

// upstreamBillingRateSyncEnabled is the probe-side pre-filter deciding whether
// a rate is even proposed for write-back. It is a necessary condition, not the
// authority: the repository CAS re-checks both switches against the row it
// updates, so a switch flipped between load and write can never sneak a rate in.
func upstreamBillingRateSyncEnabled(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamBillingRateSyncEnabledExtraKey].(bool)
	return ok && enabled && upstreamBillingProbeEnabled(account)
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

// unsupportedProbeDelay 拉长 unsupported 账号的重探间隔，让无效候选自然退出
// 热队列，不再和真正接入 sub2api 的中转账号抢每周期的探测名额。
// 仍按 upstreamBillingProbeMaxDelay 封顶，保证上游后来接入 sub2api 时最迟一天
// 内会被重新发现；base 本身已达上限（例如 Retry-After 明确要求更久）时原样返回，
// 不缩短上游指令。
func unsupportedProbeDelay(intervalMinutes int, retryAfterDuration time.Duration) time.Duration {
	base := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if base >= upstreamBillingProbeMaxDelay {
		return base
	}
	stretched := base * upstreamBillingProbeUnsupportedDelayFactor
	if stretched > upstreamBillingProbeMaxDelay {
		return upstreamBillingProbeMaxDelay
	}
	return stretched
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
