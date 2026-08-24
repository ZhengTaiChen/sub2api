package service

import (
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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	// These values live in accounts.extra so PR2 does not require a schema migration.
	UpstreamBillingProbeExtraKey           = "upstream_billing_probe"
	UpstreamBillingProbeEnabledExtraKey    = "upstream_billing_probe_enabled"
	UpstreamBillingRateSyncEnabledExtraKey = "upstream_billing_rate_sync_enabled"
	UpstreamProviderExtraKey               = "upstream_provider"
	UpstreamBillingProviderExtraKey        = "upstream_billing_provider"
	UpstreamRateMultiplierExtraKey         = "upstream_rate_multiplier"
	UpstreamRateSourceExtraKey             = "upstream_rate_source"
	UpstreamRateObservedAtExtraKey         = "upstream_rate_observed_at"
	ManualRateMultiplierExtraKey           = "manual_rate_multiplier"
	UpstreamBalanceExtraKey                = "upstream_balance"
	UpstreamBalanceUnitExtraKey            = "upstream_balance_unit"
	UpstreamBalanceTypeExtraKey            = "upstream_balance_type"
	UpstreamBalanceSourceExtraKey          = "upstream_balance_source"
	UpstreamBalanceObservedAtExtraKey      = "upstream_balance_observed_at"
	UpstreamBalanceErrorExtraKey           = "upstream_balance_error"

	upstreamBillingProbeDefaultIntervalMinutes = 30
	upstreamBillingProbeMinIntervalMinutes     = 5
	upstreamBillingProbeMaxIntervalMinutes     = 24 * 60
	upstreamBillingProbeCycleInterval          = time.Minute
	upstreamBillingProbeRequestTimeout         = 10 * time.Second
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
	upstreamBillingShuaiRequestTimeout         = 2 * time.Second
	upstreamBillingShuaiFreshTTL               = 10 * time.Minute
	upstreamBillingShuaiMaxBodyBytes           = 32 * 1024
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
)

const (
	UpstreamProviderAuto     = "auto"
	UpstreamProviderSub2API  = "sub2api"
	UpstreamProviderNewAPI   = "new_api"
	UpstreamProviderShuai    = "shuai_api"
	UpstreamProviderOpenCode = "opencode"
	UpstreamProviderCustom   = "custom"
)

// UpstreamBillingProbeSettings controls the periodic probe runner.
type UpstreamBillingProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// UpstreamBillingProbeSnapshot is persisted in accounts.extra. Data is kept as
// a sanitized map so future response fields do not require a database change.
type UpstreamBillingProbeSnapshot struct {
	Status        string         `json:"status"`
	Provider      string         `json:"provider,omitempty"`
	RateStatus    string         `json:"rate_status,omitempty"`
	RateError     string         `json:"rate_error,omitempty"`
	BalanceStatus string         `json:"balance_status,omitempty"`
	BalanceError  string         `json:"balance_error,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
	ReceivedAt    *time.Time     `json:"received_at,omitempty"`
	FreshUntil    *time.Time     `json:"fresh_until,omitempty"`
	LastAttemptAt time.Time      `json:"last_attempt_at"`
	NextProbeAt   time.Time      `json:"next_probe_at"`
	FailureCount  int            `json:"failure_count,omitempty"`
	HTTPStatus    int            `json:"http_status,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
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
	ListDueUpstreamBillingProbeAccounts(context.Context, time.Time, int) ([]Account, error)
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
	if !settings.Enabled {
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
	accounts, err := s.listDueAccounts(ctx, now)
	if err != nil {
		return fmt.Errorf("list enabled upstream billing probes: %w", err)
	}
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		if !isUpstreamBillingProbeAccount(&account) || !account.IsActive() || !upstreamBillingProbeEnabled(&account) {
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
			if _, probeErr := s.probeScheduledAccount(ctx, accountID, settings.IntervalMinutes); probeErr != nil {
				logger.LegacyPrintf("service.upstream_billing_probe", "probe_due_failed: account_id=%d err=%v", accountID, probeErr)
			}
			return nil
		})
	}
	return group.Wait()
}

func (s *UpstreamBillingProbeService) listDueAccounts(ctx context.Context, now time.Time) ([]Account, error) {
	if lister, ok := s.accountRepo.(upstreamBillingProbeDueAccountLister); ok {
		return lister.ListDueUpstreamBillingProbeAccounts(ctx, now, upstreamBillingProbeMaxPerCycle)
	}
	// Non-production repositories and older adapters keep the generic path. The
	// runner still truncates before issuing network requests.
	return s.accountRepo.FindByExtraField(ctx, UpstreamBillingProbeEnabledExtraKey, true)
}

func (s *UpstreamBillingProbeService) getSettings(ctx context.Context) (*UpstreamBillingProbeSettings, error) {
	if s.settingService == nil {
		return defaultUpstreamBillingProbeSettings(), nil
	}
	return s.settingService.GetUpstreamBillingProbeSettings(ctx)
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

// ProbeAccount performs one manual or scheduled probe. Manual calls ignore both switches.
func (s *UpstreamBillingProbeService) ProbeAccount(ctx context.Context, accountID int64) (*UpstreamBillingProbeSnapshot, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUpstreamBillingProbeUnavailable
	}
	settings, err := s.getSettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.probeAccount(ctx, accountID, settings.IntervalMinutes)
}

func (s *UpstreamBillingProbeService) probeAccount(ctx context.Context, accountID int64, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, intervalMinutes, false)
}

func (s *UpstreamBillingProbeService) probeScheduledAccount(ctx context.Context, accountID int64, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	return s.probeAccountWithMode(ctx, accountID, intervalMinutes, true)
}

func (s *UpstreamBillingProbeService) probeAccountWithMode(ctx context.Context, accountID int64, intervalMinutes int, requireEnabled bool) (*UpstreamBillingProbeSnapshot, error) {
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
		if requireEnabled {
			if !account.IsActive() || !upstreamBillingProbeEnabled(account) {
				return nil, nil
			}
			if snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra); snapshot != nil &&
				!snapshot.NextProbeAt.IsZero() && s.currentTime().Before(snapshot.NextProbeAt) {
				return nil, nil
			}
		}
		return s.probeLoadedAccount(ctx, account, intervalMinutes)
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
	var group errgroup.Group
	for i, accountID := range accountIDs {
		i, accountID := i, accountID
		results[i].AccountID = accountID
		group.Go(func() error {
			snapshot, err := s.probeAccount(ctx, accountID, settings.IntervalMinutes)
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
	if !enabled {
		// Disabling probing also disables synchronization and restores the last
		// administrator-set multiplier when the repository supports the atomic
		// account billing update path.
		if updater, ok := s.accountRepo.(AccountBillingSettingsRepository); ok {
			var manual *float64
			if value, valid := accountManualRateMultiplier(account); valid {
				manual = &value
			}
			return updater.UpdateWithAccountBillingSettings(ctx, account, boolPtr(false), boolPtr(false), manual)
		}
	}
	updates := map[string]any{UpstreamBillingProbeEnabledExtraKey: enabled}
	if !enabled {
		updates[UpstreamBillingRateSyncEnabledExtraKey] = false
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, updates)
}

func (s *UpstreamBillingProbeService) probeLoadedAccount(ctx context.Context, account *Account, intervalMinutes int) (*UpstreamBillingProbeSnapshot, error) {
	now := s.currentTime().UTC()
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "transport_unavailable", 0)
	}
	// 平台放宽后取数直读 credentials：所有 API-key 平台的密钥与自定义上游
	// 统一存放在 credentials.api_key / credentials.base_url。
	apiKey := account.GetCredential("api_key")
	if apiKey == "" {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "missing_api_key", 0)
	}
	baseURL := account.GetCredential("base_url")
	if account.IsCNProvider() && account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(APIProtocolChatCompletions)
	}
	shuaiAPI := isShuaiAPIAccount(account, baseURL)
	officialAPI := account.Platform != PlatformOpenAI && !shuaiAPI && upstreamBillingProbeTargetIsOfficialAPI(baseURL)
	if account.Platform == PlatformOpenAI && !shuaiAPI {
		if baseURL == "" {
			// 保持官方语义：OpenAI 账号无自定义 base 时探官方域（404 → unsupported）。
			baseURL = "https://api.openai.com"
		}
	}
	if officialAPI {
		// Non-OpenAI official endpoints do not expose the relay billing contract.
		// Skip both probes so an API key is never sent to a known unsupported path.
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "unsupported", 0)
	}
	normalizedBaseURL, err := s.accountTestService.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "invalid_base_url", 0)
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return s.persistProbeFailure(ctx, account, intervalMinutes, now, 0, "proxy_unavailable", 0)
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
	rateData, rateStatus, rateError, rateHTTPStatus, rateRetry := s.probeRateForAccount(ctx, account, normalizedBaseURL, proxyURL, tlsProfile, shuaiAPI, officialAPI, now)
	// Rate support is provider-specific, but balance support is intentionally
	// independent: official OpenAI-compatible endpoints may still expose
	// /v1/usage or dashboard billing.
	balanceData, balanceStatus, balanceError := s.probeBalanceForAccount(ctx, account, normalizedBaseURL, proxyURL, tlsProfile, now, shuaiAPI, rateData)

	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	data := cloneUpstreamBillingData(previousData(previous))
	for key, value := range rateData {
		data[key] = value
	}
	for key, value := range balanceData {
		data[key] = value
	}
	if balanceStatus == UpstreamBillingProbeStatusOK {
		delete(data, UpstreamBalanceErrorExtraKey)
	}
	provider := resolveUpstreamProvider(account, normalizedBaseURL, shuaiAPI, rateStatus, balanceStatus, data)

	topStatus := aggregateUpstreamProbeStatus(rateStatus, balanceStatus)
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:        topStatus,
		Provider:      provider,
		RateStatus:    rateStatus,
		RateError:     rateError,
		BalanceStatus: balanceStatus,
		BalanceError:  balanceError,
		Data:          data,
		ReceivedAt:    probeTimePtr(now),
		FreshUntil:    probeTimePtr(now.Add(2 * time.Duration(intervalMinutes) * time.Minute)),
		LastAttemptAt: now,
		NextProbeAt:   now.Add(nextProbeDelay(intervalMinutes, rateRetry)),
		HTTPStatus:    rateHTTPStatus,
	}
	if shuaiAPI {
		snapshot.FreshUntil = probeTimePtr(now.Add(upstreamBillingShuaiFreshTTL))
	}
	if topStatus == UpstreamBillingProbeStatusUnsupported {
		snapshot.LastError = "unsupported"
	} else if topStatus == UpstreamBillingProbeStatusFailed {
		// A successful capability makes the overall probe successful. Do not
		// surface the other capability's failure as a top-level failure.
		if rateStatus == UpstreamBillingProbeStatusFailed {
			snapshot.LastError = rateError
		} else {
			snapshot.LastError = balanceError
		}
	}
	if topStatus == UpstreamBillingProbeStatusOK {
		snapshot.FailureCount = 0
	} else if previous != nil {
		snapshot.FailureCount = previous.FailureCount + 1
	} else {
		snapshot.FailureCount = 1
	}

	var syncRate *float64
	previousRate := account.BillingRateMultiplier()
	if rateStatus == UpstreamBillingProbeStatusOK && upstreamBillingRateSyncEnabled(account) {
		value, valid := upstreamBillingProbeSyncRate(rateData)
		if shuaiAPI {
			value, valid = shuaiAPIUsageSyncRate(rateData)
		}
		if valid {
			syncRate = &value
			snapshot.SyncedRateMultiplier = &value
			data[UpstreamRateMultiplierExtraKey] = value
			data[UpstreamRateSourceExtraKey] = rateSourceForProvider(provider, shuaiAPI)
			data[UpstreamRateObservedAtExtraKey] = now.UTC().Format(time.RFC3339Nano)
		} else {
			declared, _ := resolveAccountExtraNumber(rateData, "resolved_rate_multiplier", "effective_rate_multiplier", "rate_multiplier")
			slog.Warn("upstream_billing_rate_sync_rejected", "source", "upstream_billing_probe", "account_id", account.ID, "declared_resolved_rate_multiplier", declared, "max_rate_multiplier", upstreamBillingRateSyncMaxMultiplier, "current_rate_multiplier", previousRate)
		}
	}
	if err := s.updateSnapshot(ctx, account, snapshot, syncRate); err != nil {
		return nil, err
	}
	if syncRate != nil {
		// 写回是后台任务的裸 SQL，不经过管理端路由，因此不会产生 audit_logs 行。
		// old_rate_multiplier 是本次探测开始时读到的值（写回的 CAS 不比对该列）。
		slog.Info("upstream_billing_rate_sync_applied",
			"source", "upstream_billing_probe",
			"account_id", account.ID,
			"old_rate_multiplier", previousRate,
			"new_rate_multiplier", *syncRate,
		)
	}
	return snapshot, nil
}

func (s *UpstreamBillingProbeService) probeRateForAccount(ctx context.Context, account *Account, baseURL, proxyURL string, tlsProfile *tlsfingerprint.Profile, shuaiAPI, officialAPI bool, now time.Time) (map[string]any, string, string, int, time.Duration) {
	if officialAPI {
		return map[string]any{}, UpstreamBillingProbeStatusUnsupported, "unsupported_rate", 0, 0
	}
	endpoint := buildOpenAIEndpointURL(baseURL, "/v1/sub2api/billing")
	if shuaiAPI {
		endpoint = buildShuaiUsageURL(baseURL)
	}
	body, status, headers, err := s.upstreamBalanceProbeGET(ctx, account, endpoint, proxyURL, tlsProfile, shuaiAPI)
	if err != nil {
		if err.Error() == "empty response" {
			return map[string]any{}, UpstreamBillingProbeStatusFailed, "empty_response", status, 0
		}
		return map[string]any{}, UpstreamBillingProbeStatusFailed, "request_failed", status, 0
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return map[string]any{}, UpstreamBillingProbeStatusUnsupported, "unsupported_rate", status, 0
	}
	if status < 200 || status >= 300 {
		return map[string]any{}, UpstreamBillingProbeStatusFailed, "http_error", status, retryAfter(headers, now)
	}
	var data map[string]any
	if shuaiAPI {
		data, err = parseShuaiAPIUsageResponse(body)
	} else {
		data, err = parseUpstreamBillingProbeResponse(body)
	}
	if err != nil {
		return map[string]any{}, UpstreamBillingProbeStatusUnsupported, "unsupported_rate", status, 0
	}
	if _, hasRate := data["resolved_rate_multiplier"]; !hasRate {
		return data, UpstreamBillingProbeStatusUnsupported, "missing_rate", status, 0
	}
	data["rate_source"] = rateSourceForProvider(resolveUpstreamProvider(account, baseURL, shuaiAPI, UpstreamBillingProbeStatusOK, UpstreamBillingProbeStatusUnsupported, data), shuaiAPI)
	return data, UpstreamBillingProbeStatusOK, "", status, 0
}

func previousData(snapshot *UpstreamBillingProbeSnapshot) map[string]any {
	if snapshot == nil {
		return nil
	}
	return snapshot.Data
}

func cloneUpstreamBillingData(data map[string]any) map[string]any {
	cloned := make(map[string]any, len(data)+8)
	for key, value := range data {
		cloned[key] = value
	}
	return cloned
}

func aggregateUpstreamProbeStatus(rateStatus, balanceStatus string) string {
	if rateStatus == UpstreamBillingProbeStatusOK || balanceStatus == UpstreamBillingProbeStatusOK {
		return UpstreamBillingProbeStatusOK
	}
	if rateStatus == UpstreamBillingProbeStatusUnsupported && balanceStatus == UpstreamBillingProbeStatusUnsupported {
		return UpstreamBillingProbeStatusUnsupported
	}
	return UpstreamBillingProbeStatusFailed
}

func rateSourceForProvider(provider string, shuaiAPI bool) string {
	if shuaiAPI || provider == UpstreamProviderShuai {
		return "shuai_api_usage"
	}
	return "sub2api_billing"
}

func resolveUpstreamProvider(account *Account, baseURL string, shuaiAPI bool, rateStatus, balanceStatus string, data map[string]any) string {
	valid := func(value string) string {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case UpstreamProviderSub2API, UpstreamProviderNewAPI, UpstreamProviderShuai, UpstreamProviderOpenCode, UpstreamProviderCustom:
			return strings.ToLower(strings.TrimSpace(value))
		default:
			return ""
		}
	}
	if account != nil && account.Extra != nil {
		value, _ := account.Extra[UpstreamProviderExtraKey].(string)
		if value == "" {
			value, _ = account.Extra[UpstreamBillingProviderExtraKey].(string)
		}
		if strings.ToLower(strings.TrimSpace(value)) != UpstreamProviderAuto {
			if provider := valid(value); provider != "" {
				return provider
			}
		}
	}
	if account != nil {
		if provider := valid(account.GetCredential("provider")); provider != "" {
			return provider
		}
	}
	if shuaiAPI {
		return UpstreamProviderShuai
	}
	if rateStatus == UpstreamBillingProbeStatusOK {
		return UpstreamProviderSub2API
	}
	if source, _ := data[UpstreamBalanceSourceExtraKey].(string); strings.Contains(source, "dashboard") || balanceStatus == UpstreamBillingProbeStatusOK && source == "openai_dashboard" {
		return UpstreamProviderNewAPI
	}
	if parsed, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && strings.Contains(strings.ToLower(parsed.Hostname()), "opencode.ai") {
		return UpstreamProviderOpenCode
	}
	return UpstreamProviderCustom
}

func (s *UpstreamBillingProbeService) persistProbeFailure(
	ctx context.Context,
	account *Account,
	intervalMinutes int,
	now time.Time,
	statusCode int,
	reason string,
	retryAfterDuration time.Duration,
) (*UpstreamBillingProbeSnapshot, error) {
	previous := decodeUpstreamBillingProbeSnapshot(account.Extra)
	failureCount := 1
	if previous != nil {
		failureCount = previous.FailureCount + 1
	}
	status := UpstreamBillingProbeStatusFailed
	delay := nextProbeDelay(intervalMinutes, retryAfterDuration)
	if reason == "unsupported" {
		status = UpstreamBillingProbeStatusUnsupported
		delay = unsupportedProbeDelay(intervalMinutes, retryAfterDuration)
	}
	snapshot := &UpstreamBillingProbeSnapshot{
		Status:        status,
		RateStatus:    status,
		RateError:     reason,
		BalanceStatus: status,
		BalanceError:  reason,
		LastAttemptAt: now,
		NextProbeAt:   now.Add(delay),
		FailureCount:  failureCount,
		HTTPStatus:    statusCode,
		LastError:     reason,
	}
	if previous != nil {
		snapshot.Data = cloneUpstreamBillingData(previous.Data)
		snapshot.Provider = previous.Provider
		snapshot.ReceivedAt = previous.ReceivedAt
		snapshot.FreshUntil = previous.FreshUntil
		if snapshot.FreshUntil == nil && previous.Status == UpstreamBillingProbeStatusOK && previous.ReceivedAt != nil {
			snapshot.FreshUntil = probeTimePtr(previous.ReceivedAt.Add(2 * time.Duration(intervalMinutes) * time.Minute))
		}
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

func upstreamBillingBalanceObservedAt(now time.Time) string {
	return now.UTC().Format(time.RFC3339Nano)
}

func upstreamBalanceSnapshotData(
	data map[string]any,
	balance float64,
	unit string,
	balanceType string,
	source string,
	now time.Time,
) {
	if data == nil {
		return
	}
	data[UpstreamBalanceExtraKey] = balance
	data[UpstreamBalanceUnitExtraKey] = unit
	data[UpstreamBalanceTypeExtraKey] = balanceType
	data[UpstreamBalanceSourceExtraKey] = source
	data[UpstreamBalanceObservedAtExtraKey] = upstreamBillingBalanceObservedAt(now)
	delete(data, UpstreamBalanceErrorExtraKey)
}

func upstreamBalanceUnsupported(
	data map[string]any,
	reason string,
	now time.Time,
) {
	if data == nil {
		return
	}
	data[UpstreamBalanceObservedAtExtraKey] = upstreamBillingBalanceObservedAt(now)
	data[UpstreamBalanceErrorExtraKey] = reason
}

func firstUsageBalanceValue(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if balance, valid := finiteJSONNumber(value); valid {
			return balance, true
		}
	}
	return 0, false
}

func parseUpstreamUsageBalance(body []byte) (float64, string, string, bool) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, "", "", false
	}
	if valid, ok := response["isValid"].(bool); ok && !valid {
		return 0, "", "", false
	}
	if active, ok := response["is_active"].(bool); ok && !active {
		return 0, "", "", false
	}
	balance, source, ok := findUpstreamUsageBalance(response)
	if !ok {
		return 0, "", "", false
	}
	unit := firstUpstreamString(source, response, "unit", "currency")
	if strings.TrimSpace(unit) == "" {
		unit = "USD"
	}
	balanceType := "wallet"
	if mode := firstUpstreamString(source, response, "mode"); mode == "quota_limited" {
		balanceType = "quota_remaining"
	} else if _, ok := source["subscription"].(map[string]any); ok {
		balanceType = "subscription"
	} else if _, ok := response["subscription"].(map[string]any); ok {
		balanceType = "subscription"
	}
	return balance, strings.TrimSpace(unit), balanceType, true
}

// findUpstreamUsageBalance handles bounded envelopes used by compatible relays.
func findUpstreamUsageBalance(response map[string]any) (float64, map[string]any, bool) {
	if response == nil {
		return 0, nil, false
	}
	queue := []map[string]any{response}
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		if candidate == nil {
			continue
		}
		if balance, ok := firstUsageBalanceValue(candidate, "remaining", "balance", "available_balance", "total_available", "total_balance", "credit"); ok {
			return balance, candidate, true
		}
		for _, key := range []string{"data", "quota", "usage", "subscription"} {
			if nested, ok := candidate[key].(map[string]any); ok {
				queue = append(queue, nested)
			}
		}
		if len(queue) > 16 {
			queue = queue[:16]
		}
	}
	return 0, nil, false
}

func firstUpstreamString(primary, fallback map[string]any, keys ...string) string {
	for _, values := range []map[string]any{primary, fallback} {
		for _, key := range keys {
			if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func parseUpstreamOpenAIBilling(body []byte) (float64, bool) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, false
	}
	limit, ok := findUpstreamNumber(response, "hard_limit_usd", "hard_limit", "limit")
	if !ok || limit < 0 || math.IsNaN(limit) || math.IsInf(limit, 0) || limit > 1000000 {
		return 0, false
	}
	return limit, true
}

func parseUpstreamOpenAIUsage(body []byte) (float64, bool) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, false
	}
	usage, ok := findUpstreamNumber(response, "total_usage")
	if !ok || usage < 0 || math.IsNaN(usage) || math.IsInf(usage, 0) {
		return 0, false
	}
	return usage, true
}

func findUpstreamNumber(response map[string]any, keys ...string) (float64, bool) {
	if response == nil {
		return 0, false
	}
	queue := []map[string]any{response}
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		for _, key := range keys {
			if value, exists := candidate[key]; exists {
				if number, ok := finiteJSONNumber(value); ok {
					return number, true
				}
			}
		}
		for _, key := range []string{"data", "usage", "subscription"} {
			if nested, ok := candidate[key].(map[string]any); ok {
				queue = append(queue, nested)
			}
		}
		if len(queue) > 16 {
			queue = queue[:16]
		}
	}
	return 0, false
}

func (s *UpstreamBillingProbeService) upstreamBalanceProbeGET(
	ctx context.Context,
	account *Account,
	probeURL string,
	proxyURL string,
	tlsProfile *tlsfingerprint.Profile,
	shuaiAPI bool,
) ([]byte, int, http.Header, error) {
	timeout := upstreamBillingProbeRequestTimeout
	maxBodyBytes := upstreamBillingProbeMaxBodyBytes
	if shuaiAPI {
		timeout = upstreamBillingShuaiRequestTimeout
		maxBodyBytes = upstreamBillingShuaiMaxBodyBytes
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, bytes.NewReader(nil))
	if err != nil {
		return nil, 0, nil, err
	}
	profile := HTTPUpstreamProfileDefault
	if account.Platform == PlatformOpenAI {
		profile = HTTPUpstreamProfileOpenAI
	}
	reqCtx := WithHTTPUpstreamProfile(req.Context(), profile)
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(reqCtx))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	if shuaiAPI {
		req.Header.Set("User-Agent", "cc-switch/1.0")
	}
	account.ApplyHeaderOverrides(req.Header)
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, 0, nil, fmt.Errorf("empty response")
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes)+1))
	if readErr != nil {
		return nil, resp.StatusCode, resp.Header, readErr
	}
	if len(body) > maxBodyBytes {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("response too large")
	}
	return body, resp.StatusCode, resp.Header, nil
}

func (s *UpstreamBillingProbeService) probeBalanceForAccount(
	ctx context.Context,
	account *Account,
	baseURL string,
	proxyURL string,
	tlsProfile *tlsfingerprint.Profile,
	now time.Time,
	shuaiAPI bool,
	rateData map[string]any,
) (map[string]any, string, string) {
	result := make(map[string]any)
	if shuaiAPI {
		if balance, ok := firstUsageBalanceValue(rateData, UpstreamBalanceExtraKey, "remaining", "balance", "available_balance", "total_available"); ok {
			upstreamBalanceSnapshotData(result, balance, "USD", "quota_remaining", "shuai_api_usage", now)
			return result, UpstreamBillingProbeStatusOK, ""
		}
		return result, UpstreamBillingProbeStatusUnsupported, "missing_balance"
	}
	usageURL := buildOpenAIEndpointURL(baseURL, "/v1/usage")
	body, usageStatus, _, err := s.upstreamBalanceProbeGET(ctx, account, usageURL, proxyURL, tlsProfile, false)
	if err == nil && usageStatus >= 200 && usageStatus < 300 {
		if balance, unit, balanceType, ok := parseUpstreamUsageBalance(body); ok {
			upstreamBalanceSnapshotData(result, balance, unit, balanceType, "usage_endpoint", now)
			return result, UpstreamBillingProbeStatusOK, ""
		}
	}
	subscriptionURL := buildOpenAIEndpointURL(baseURL, "/v1/dashboard/billing/subscription")
	subscriptionBody, subscriptionStatus, _, subErr := s.upstreamBalanceProbeGET(ctx, account, subscriptionURL, proxyURL, tlsProfile, false)
	startDate := now.UTC().Format("2006-01") + "-01"
	endDate := now.UTC().Format("2006-01-02")
	usageDashboardURL := buildOpenAIEndpointURL(baseURL, "/v1/dashboard/billing/usage") + "?start_date=" + url.QueryEscape(startDate) + "&end_date=" + url.QueryEscape(endDate)
	usageBody, usageDashboardStatus, _, usageErr := s.upstreamBalanceProbeGET(ctx, account, usageDashboardURL, proxyURL, tlsProfile, false)
	if subErr == nil && usageErr == nil {
		if limit, ok := parseUpstreamOpenAIBilling(subscriptionBody); ok {
			if usageCents, usageOK := parseUpstreamOpenAIUsage(usageBody); usageOK {
				balance := limit - usageCents/100
				upstreamBalanceSnapshotData(result, balance, "USD", "subscription", "openai_dashboard", now)
				return result, UpstreamBillingProbeStatusOK, ""
			}
		}
	}
	if usageErr != nil || subErr != nil {
		return result, UpstreamBillingProbeStatusFailed, "request_failed"
	} else if isUnsupportedUpstreamStatus(usageStatus) && isUnsupportedUpstreamStatus(subscriptionStatus) && isUnsupportedUpstreamStatus(usageDashboardStatus) {
		return result, UpstreamBillingProbeStatusUnsupported, "unsupported_balance"
	} else {
		return result, UpstreamBillingProbeStatusFailed, "invalid_response"
	}
}

func isUnsupportedUpstreamStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed
}

// isShuaiAPIAccount identifies the 帅API usage endpoint without requiring a
// schema migration. The explicit provider marker is useful for custom domains
// and is intentionally checked before host matching.
func isShuaiAPIAccount(account *Account, baseURL string) bool {
	if account != nil {
		for _, value := range []any{account.GetCredential("provider"), account.Extra[UpstreamProviderExtraKey], account.Extra[UpstreamBillingProviderExtraKey]} {
			if provider, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(provider), UpstreamProviderShuai) {
				return true
			}
		}
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return host == "shuaiapi.com" || strings.HasSuffix(host, ".shuaiapi.com")
}

func buildShuaiUsageURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/api/usage/token/"
	}
	path := strings.TrimRight(parsed.Path, "/")
	// Account forms commonly store an OpenAI-compatible /v1 base. The 帅API
	// endpoint is rooted at /api, so avoid producing /v1/api/usage/token/.
	if strings.HasSuffix(strings.ToLower(path), "/v1") {
		path = path[:len(path)-len("/v1")]
	}
	parsed.Path = strings.TrimRight(path, "/") + "/api/usage/token/"
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}

// parseShuaiAPIUsageResponse keeps only the small, administrator-visible
// snapshot needed for balance/rate display. Credentials and arbitrary upstream
// fields are deliberately discarded. A malformed rate does not invalidate an
// otherwise usable balance response.
func parseShuaiAPIUsageResponse(body []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	code, ok := root["code"].(bool)
	if !ok || !code {
		return nil, fmt.Errorf("invalid shuai api response code")
	}
	data, ok := root["data"].(map[string]any)
	if !ok || data == nil {
		return nil, fmt.Errorf("missing shuai api response data")
	}
	if object, exists := data["object"]; exists {
		if value, valid := object.(string); valid && value != "" && value != "token_usage" {
			return nil, fmt.Errorf("unexpected shuai api response object")
		}
	}

	result := map[string]any{"billing_provider": "shuai_api", "rate_source": "shuai_api_usage"}
	if object, ok := data["object"].(string); ok && object != "" {
		result["object"] = object
	}
	if display, ok := data["display"].(map[string]any); ok {
		if remaining, valid := finiteJSONNumber(display["remaining"]); valid {
			result["balance"] = remaining
			result["unit"] = "USD"
		}
	}
	if _, exists := result["balance"]; !exists {
		if available, valid := finiteJSONNumber(data["total_available"]); valid {
			result["balance"] = available / 500000
			result["unit"] = "USD"
		} else {
			granted, grantedOK := finiteJSONNumber(data["total_granted"])
			used, usedOK := finiteJSONNumber(data["total_used"])
			if grantedOK && usedOK {
				result["balance"] = (granted - used) / 500000
				result["unit"] = "USD"
			}
		}
	}

	// Upstream aliases are accepted in the documented order. Invalid values are
	// ignored so balance display remains useful and does not become a zero-rate
	// write-back.
	rateKeys := []string{"effective_rate_multiplier", "resolved_rate_multiplier", "rate_multiplier", "multiplier", "rate"}
	for _, key := range rateKeys {
		if value, valid := finiteNonNegativeJSONNumber(data[key]); valid {
			result[key] = value
			result["effective_rate_multiplier"] = value
			break
		}
	}
	if _, exists := result["effective_rate_multiplier"]; !exists {
		for _, key := range rateKeys {
			if value, valid := finiteNonNegativeJSONNumber(root[key]); valid {
				result[key] = value
				result["effective_rate_multiplier"] = value
				break
			}
		}
	}
	// 帅API没有高峰倍率，且现有账号列表把它作为 token 级上游声明展示；
	// 补上 billing_scope/resolved_rate_multiplier 让旧的“上游声明倍率”列也能显示。
	if rate, ok := result["effective_rate_multiplier"]; ok {
		if value, valid := finiteNonNegativeJSONNumber(rate); valid {
			result["billing_scope"] = "token"
			result["resolved_rate_multiplier"] = value
			result["group_rate_multiplier"] = value
			result["peak_rate_enabled"] = false
		}
	}
	if observedAt, ok := data["observed_at"].(string); ok && strings.TrimSpace(observedAt) != "" {
		result["observed_at"] = observedAt
	}
	return result, nil
}

func finiteNonNegativeJSONNumber(value any) (float64, bool) {
	number, ok := finiteJSONNumber(value)
	return number, ok && number >= 0
}

func finiteJSONNumber(value any) (float64, bool) {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case float32:
		number = float64(v)
	case int:
		number = float64(v)
	case int64:
		number = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func shuaiAPIUsageSyncRate(data map[string]any) (float64, bool) {
	value, ok := resolveAccountExtraNumber(data, "effective_rate_multiplier")
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, false
	}
	rounded := math.Round(value*upstreamBillingProbeAccountRateScale) / upstreamBillingProbeAccountRateScale
	if rounded <= 0 || rounded > upstreamBillingRateSyncMaxMultiplier {
		return 0, false
	}
	return rounded, true
}

func accountManualRateMultiplier(account *Account) (float64, bool) {
	if account == nil {
		return 0, false
	}
	if value, ok := resolveAccountExtraNumber(account.Extra, ManualRateMultiplierExtraKey); ok &&
		!math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
		return value, true
	}
	// When sync is active, the database column may contain the last upstream
	// value rather than an administrator-owned value. Without an explicit
	// backup, let the repository restore its documented 1.0 default instead of
	// accidentally treating that upstream value as the manual fallback.
	if upstreamBillingRateSyncEnabled(account) {
		return 0, false
	}
	if account.RateMultiplier != nil && *account.RateMultiplier >= 0 &&
		!math.IsNaN(*account.RateMultiplier) && !math.IsInf(*account.RateMultiplier, 0) {
		return *account.RateMultiplier, true
	}
	return 0, false
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
// sub2api convention shared by the supported API-key platforms (including the
// CN providers, whose official-domain accounts are short-circuited to
// "unsupported" by upstreamBillingProbeTargetIsOfficialAPI).
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
	case PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek:
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
// CN provider domains (moonshot.cn / kimi.com / bigmodel.cn / deepseek.com)
// serve the same role: official APIs that can never host /v1/sub2api/billing,
// so their accounts short-circuit to "unsupported" without a request.
var upstreamBillingProbeOfficialAPIDomains = []string{
	"anthropic.com",
	"googleapis.com",
	"x.ai",
	"grok.com",
	"openai.com",
	"ollama.com",
	"moonshot.cn",
	"kimi.com",
	"bigmodel.cn",
	"deepseek.com",
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
	if account == nil || account.Extra == nil {
		return false
	}
	enabled, ok := account.Extra[UpstreamBillingProbeEnabledExtraKey].(bool)
	return ok && enabled
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
