package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAIAccountRuntimePersistenceTestReport struct {
	accountID    int64
	model        string
	success      bool
	firstTokenMs *int
	observedAt   time.Time
	ttl          time.Duration
}

type openAIAccountRuntimePersistenceTestStore struct {
	reportStarted chan struct{}
	reportRelease chan struct{}
	reportStart   sync.Once
	reportErr     error

	reportsMu sync.Mutex
	reports   []openAIAccountRuntimePersistenceTestReport

	loadStarted chan struct{}
	loadRelease chan struct{}
	loadStart   sync.Once
	loadCalls   atomic.Int64
	loadErr     error
	account     OpenAIAccountRuntimeStatSnapshot
	model       OpenAIAccountRuntimeStatSnapshot
	loadFunc    func(accountID int64, model string) (OpenAIAccountRuntimeStatSnapshot, OpenAIAccountRuntimeStatSnapshot, error)
}

func (s *openAIAccountRuntimePersistenceTestStore) Report(
	ctx context.Context,
	accountID int64,
	model string,
	success bool,
	firstTokenMs *int,
	observedAt time.Time,
	ttl time.Duration,
) error {
	if s.reportStarted != nil {
		s.reportStart.Do(func() { close(s.reportStarted) })
	}
	if s.reportRelease != nil {
		select {
		case <-s.reportRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var copiedTTFT *int
	if firstTokenMs != nil {
		value := *firstTokenMs
		copiedTTFT = &value
	}
	s.reportsMu.Lock()
	s.reports = append(s.reports, openAIAccountRuntimePersistenceTestReport{
		accountID:    accountID,
		model:        model,
		success:      success,
		firstTokenMs: copiedTTFT,
		observedAt:   observedAt,
		ttl:          ttl,
	})
	s.reportsMu.Unlock()
	return s.reportErr
}

func (s *openAIAccountRuntimePersistenceTestStore) Load(
	ctx context.Context,
	accountID int64,
	model string,
) (OpenAIAccountRuntimeStatSnapshot, OpenAIAccountRuntimeStatSnapshot, error) {
	s.loadCalls.Add(1)
	if s.loadStarted != nil {
		s.loadStart.Do(func() { close(s.loadStarted) })
	}
	if s.loadRelease != nil {
		select {
		case <-s.loadRelease:
		case <-ctx.Done():
			return OpenAIAccountRuntimeStatSnapshot{}, OpenAIAccountRuntimeStatSnapshot{}, ctx.Err()
		}
	}
	if s.loadFunc != nil {
		return s.loadFunc(accountID, model)
	}
	return s.account, s.model, s.loadErr
}

func (s *openAIAccountRuntimePersistenceTestStore) snapshotReports() []openAIAccountRuntimePersistenceTestReport {
	s.reportsMu.Lock()
	defer s.reportsMu.Unlock()
	result := make([]openAIAccountRuntimePersistenceTestReport, len(s.reports))
	copy(result, s.reports)
	return result
}

func testOpenAIAccountRuntimePersistenceOptions() openAIAccountRuntimePersistenceOptions {
	opts := defaultOpenAIAccountRuntimePersistenceOptions()
	opts.writeQueueSize = 1
	opts.loadQueueSize = 1
	opts.redisTimeout = 2 * time.Second
	opts.refreshInterval = 100 * time.Millisecond
	return opts
}

func TestOpenAIAccountRuntimePersistenceReportCopiesTTFTAndNeverBlocksOnFullQueue(t *testing.T) {
	store := &openAIAccountRuntimePersistenceTestStore{
		reportStarted: make(chan struct{}),
		reportRelease: make(chan struct{}),
	}
	persistence := newOpenAIAccountRuntimePersistenceWithOptions(store, testOpenAIAccountRuntimePersistenceOptions())
	released := false
	t.Cleanup(func() {
		if !released {
			close(store.reportRelease)
		}
		persistence.Stop()
	})

	observedAt := time.Date(2026, time.August, 15, 8, 0, 0, 0, time.UTC)
	ttft := 1234
	require.True(t, persistence.Report(41, "  GPT-5.6  ", true, &ttft, observedAt))
	select {
	case <-store.reportStarted:
	case <-time.After(time.Second):
		t.Fatal("write worker did not start")
	}
	ttft = 9999
	queuedTTFT := 2345
	require.True(t, persistence.Report(42, "gpt-5.6", true, &queuedTTFT, observedAt.Add(time.Second)))
	start := time.Now()
	require.False(t, persistence.Report(43, "gpt-5.6", false, nil, observedAt.Add(2*time.Second)))
	require.Less(t, time.Since(start), 100*time.Millisecond)
	require.Equal(t, uint64(1), persistence.Snapshot().DroppedWrites)

	close(store.reportRelease)
	released = true
	require.Eventually(t, func() bool {
		return len(store.snapshotReports()) == 2
	}, time.Second, 10*time.Millisecond)
	persistence.Stop()
	reports := store.snapshotReports()
	require.Len(t, reports, 2)
	require.NotNil(t, reports[0].firstTokenMs)
	require.Equal(t, 1234, *reports[0].firstTokenMs)
	require.Equal(t, "gpt-5.6", reports[0].model)
	require.Equal(t, openAIAccountRuntimePersistenceReportTTL, reports[0].ttl)
}

func TestOpenAIAccountRuntimePersistenceLoadIsAsyncDeduplicatedAndThrottled(t *testing.T) {
	now := time.Now()
	store := &openAIAccountRuntimePersistenceTestStore{
		loadStarted: make(chan struct{}),
		loadRelease: make(chan struct{}),
		account: OpenAIAccountRuntimeStatSnapshot{
			ErrorRateEWMA: 0.1,
			TTFTEWMA:      1800,
			SampleCount:   4,
			TTFTUpdatedAt: now,
			LastSuccessAt: now,
		},
	}
	persistence := newOpenAIAccountRuntimePersistenceWithOptions(store, testOpenAIAccountRuntimePersistenceOptions())
	released := false
	t.Cleanup(func() {
		if !released {
			close(store.loadRelease)
		}
		persistence.Stop()
	})

	result := make(chan error, 2)
	apply := func(
		_ int64,
		_ string,
		account OpenAIAccountRuntimeStatSnapshot,
		_ OpenAIAccountRuntimeStatSnapshot,
		err error,
	) {
		if err == nil && account.TTFTEWMA != 1800 {
			err = errors.New("unexpected account snapshot")
		}
		result <- err
	}
	require.True(t, persistence.EnsureLoaded(51, "GPT-5.6", apply))
	select {
	case <-store.loadStarted:
	case <-time.After(time.Second):
		t.Fatal("load worker did not start")
	}
	require.False(t, persistence.EnsureLoaded(51, "gpt-5.6", apply))
	require.False(t, persistence.Refresh(51, "gpt-5.6", apply))
	close(store.loadRelease)
	released = true
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("load callback did not run")
	}
	require.False(t, persistence.EnsureLoaded(51, "gpt-5.6", apply))
	require.False(t, persistence.Refresh(51, "gpt-5.6", apply))

	time.Sleep(120 * time.Millisecond)
	require.True(t, persistence.Refresh(51, "gpt-5.6", apply))
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("refresh callback did not run")
	}
	require.Equal(t, int64(2), store.loadCalls.Load())
}

func TestOpenAIAccountRuntimePersistenceLoadErrorIsThrottledAndStopIsIdempotent(t *testing.T) {
	store := &openAIAccountRuntimePersistenceTestStore{loadErr: errors.New("redis unavailable")}
	opts := testOpenAIAccountRuntimePersistenceOptions()
	opts.refreshInterval = time.Hour
	persistence := newOpenAIAccountRuntimePersistenceWithOptions(store, opts)
	result := make(chan error, 1)
	require.True(t, persistence.EnsureLoaded(61, "gpt-5.6", func(
		_ int64,
		_ string,
		_ OpenAIAccountRuntimeStatSnapshot,
		_ OpenAIAccountRuntimeStatSnapshot,
		err error,
	) {
		result <- err
	}))
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("load error callback did not run")
	}
	require.False(t, persistence.Refresh(61, "gpt-5.6", nil))
	require.Equal(t, int64(1), store.loadCalls.Load())

	persistence.Stop()
	persistence.Stop()
	require.False(t, persistence.Report(61, "gpt-5.6", true, nil, time.Now()))
	require.False(t, persistence.EnsureLoaded(62, "gpt-5.6", nil))
}

func TestMergeOpenAIAccountRuntimeSnapshotsUsesNewerEvidenceOnly(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	accountID := int64(71)
	model := "gpt-5.6"
	localAt := time.Date(2026, time.August, 15, 9, 0, 0, 0, time.UTC)
	localTTFT := 1100
	stats.reportAt(accountID, true, &localTTFT, localAt, model)

	older := OpenAIAccountRuntimeStatSnapshot{
		ErrorRateEWMA: 1,
		TTFTEWMA:      9000,
		SampleCount:   20,
		TTFTUpdatedAt: localAt.Add(-time.Second),
		LastFailureAt: localAt.Add(-time.Second),
	}
	require.False(t, mergeOpenAIAccountRuntimeSnapshots(stats, accountID, model, older, older))
	errorRate, ttft, hasTTFT, sampleCount, updatedAt := stats.snapshotWithMeta(accountID)
	require.Zero(t, errorRate)
	require.True(t, hasTTFT)
	require.Equal(t, 1100.0, ttft)
	require.Equal(t, int64(1), sampleCount)
	require.True(t, localAt.Equal(updatedAt))

	remoteAt := localAt.Add(time.Minute)
	newer := OpenAIAccountRuntimeStatSnapshot{
		ErrorRateEWMA: 0.4,
		TTFTEWMA:      2200,
		SampleCount:   8,
		TTFTUpdatedAt: remoteAt,
		LastFailureAt: remoteAt,
		LastSuccessAt: remoteAt.Add(-time.Second),
	}
	require.True(t, mergeOpenAIAccountRuntimeSnapshots(stats, accountID, model, newer, newer))
	errorRate, ttft, hasTTFT, sampleCount, updatedAt = stats.snapshotWithMeta(accountID)
	require.InDelta(t, 0.4, errorRate, 1e-12)
	require.True(t, hasTTFT)
	require.Equal(t, 2200.0, ttft)
	require.Equal(t, int64(8), sampleCount)
	require.True(t, remoteAt.Equal(updatedAt))
	recentFailure, failureAt := openAIAccountRuntimeStatRecentFailure(stats.loadOrCreate(accountID), remoteAt)
	require.True(t, recentFailure)
	require.True(t, remoteAt.Equal(failureAt))

	_, modelTTFT, modelHasTTFT, modelSamples, modelUpdatedAt := stats.snapshotModelWithMeta(accountID, model)
	require.True(t, modelHasTTFT)
	require.Equal(t, 2200.0, modelTTFT)
	require.Equal(t, int64(8), modelSamples)
	require.True(t, remoteAt.Equal(modelUpdatedAt))
}

func TestOpenAIAccountRuntimeStatsIgnoreOutOfOrderLocalReport(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	fastTTFT := 900
	slowTTFT := 30000
	stats.reportAt(75, true, &fastTTFT, now, "gpt-5.6")
	stats.reportAt(75, false, &slowTTFT, now.Add(-time.Second), "gpt-5.6")

	errorRate, ttft, hasTTFT, samples, updatedAt := stats.snapshotWithMeta(75)
	require.Zero(t, errorRate)
	require.True(t, hasTTFT)
	require.Equal(t, 900.0, ttft)
	require.Equal(t, int64(1), samples)
	require.True(t, now.Equal(updatedAt))
	recentFailure, _ := openAIAccountRuntimeStatRecentFailure(stats.loadOrCreate(75), now)
	require.False(t, recentFailure)
}

func TestOpenAIAccountRuntimeStatsNewerSuccessClearsRecentFailure(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	failedAt := time.Date(2026, time.August, 15, 9, 30, 0, 123_456_000, time.UTC)
	succeededAt := failedAt.Add(time.Microsecond)
	ttft := 950

	stats.reportAt(76, false, nil, failedAt, "gpt-5.6")
	stats.reportAt(76, true, &ttft, succeededAt, "gpt-5.6")

	recentFailure, _ := openAIAccountRuntimeStatRecentFailure(stats.loadOrCreate(76), succeededAt)
	require.False(t, recentFailure)
	errorRate, gotTTFT, hasTTFT, samples, updatedAt := stats.snapshotWithMeta(76)
	require.InDelta(t, 0.16, errorRate, 1e-12)
	require.True(t, hasTTFT)
	require.Equal(t, float64(ttft), gotTTFT)
	require.Equal(t, int64(1), samples)
	require.True(t, succeededAt.Equal(updatedAt))
}

func TestOpenAIGatewayRuntimePersistenceHydratesRankingAfterRestart(t *testing.T) {
	now := time.Now().UTC()
	probeWinnerButSlow := openAIPoolProbeTestAccount(81, 0, UpstreamBillingProbeStatusOK, 1000)
	probeBackup := openAIPoolProbeTestAccount(82, 20, UpstreamBillingProbeStatusOK, 2200)
	persistedSlow := OpenAIAccountRuntimeStatSnapshot{
		ErrorRateEWMA: 0,
		TTFTEWMA:      8000,
		SampleCount:   openAIPoolProbeRuntimeSamples,
		TTFTUpdatedAt: now,
		LastSuccessAt: now,
	}
	store := &openAIAccountRuntimePersistenceTestStore{
		loadFunc: func(accountID int64, _ string) (OpenAIAccountRuntimeStatSnapshot, OpenAIAccountRuntimeStatSnapshot, error) {
			if accountID == probeWinnerButSlow.ID {
				return persistedSlow, persistedSlow, nil
			}
			return OpenAIAccountRuntimeStatSnapshot{}, OpenAIAccountRuntimeStatSnapshot{}, nil
		},
	}
	persistence := newOpenAIAccountRuntimePersistenceWithOptions(store, testOpenAIAccountRuntimePersistenceOptions())
	t.Cleanup(persistence.Stop)
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{probeWinnerButSlow, probeBackup}},
		cfg:         &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{loadMap: map[int64]*AccountLoadInfo{
			probeWinnerButSlow.ID: {AccountID: probeWinnerButSlow.ID},
			probeBackup.ID:        {AccountID: probeBackup.ID},
		}}),
		openaiAccountStats:       newOpenAIAccountRuntimeStats(),
		openaiRuntimePersistence: persistence,
	}

	// The first call schedules a non-blocking hydrate. Once Redis has replied,
	// the same ranking path must put the genuinely faster backup first.
	_ = svc.BuildPoolAutoPriorityRanking([]*Account{&probeWinnerButSlow, &probeBackup}, now)
	require.Eventually(t, func() bool {
		ranking := svc.BuildPoolAutoPriorityRanking([]*Account{&probeWinnerButSlow, &probeBackup}, now)
		if len(ranking) != 2 || ranking[0].AccountID != probeBackup.ID ||
			ranking[1].RankingSource != PoolAutoPriorityRankingSourceRealTraffic {
			return false
		}
		selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "gpt-5.6-sol", nil)
		if err != nil || selection == nil {
			return false
		}
		if selection.ReleaseFunc != nil {
			defer selection.ReleaseFunc()
		}
		return selection.Account != nil && selection.Account.ID == ranking[0].AccountID
	}, time.Second, 10*time.Millisecond)
}
