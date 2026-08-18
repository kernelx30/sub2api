package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newOpenAIAccountRuntimeStatsTestStore(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache, ok := NewGatewayCache(rdb).(*gatewayCache)
	require.True(t, ok)
	_, ok = any(cache).(service.OpenAIAccountRuntimeStatsStore)
	require.True(t, ok)
	return cache, mr
}

func TestOpenAIAccountRuntimeStatsStoreMissReturnsZeroSnapshots(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	account, model, err := store.Load(context.Background(), 41, "  GPT-5.6  ")
	require.NoError(t, err)
	require.Equal(t, service.OpenAIAccountRuntimeStatSnapshot{}, account)
	require.Equal(t, service.OpenAIAccountRuntimeStatSnapshot{}, model)
}

func TestOpenAIAccountRuntimeStatsStoreEWMAAndTimestamps(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	ttl := 15 * time.Minute
	firstObservedAt := time.Date(2026, time.August, 15, 1, 2, 3, 400_001_000, time.UTC)
	secondObservedAt := firstObservedAt.Add(time.Second)
	failedAt := secondObservedAt.Add(time.Second)
	firstTTFT := 1000
	secondTTFT := 3000

	require.NoError(t, store.Report(ctx, 42, "  GPT-5.6  ", true, &firstTTFT, firstObservedAt, ttl))
	account, model, err := store.Load(ctx, 42, "gpt-5.6")
	require.NoError(t, err)
	for _, snapshot := range []service.OpenAIAccountRuntimeStatSnapshot{account, model} {
		require.Zero(t, snapshot.ErrorRateEWMA)
		require.Equal(t, 1000.0, snapshot.TTFTEWMA)
		require.Equal(t, int64(1), snapshot.SampleCount)
		require.Equal(t, firstObservedAt, snapshot.TTFTUpdatedAt)
		require.Equal(t, firstObservedAt, snapshot.LastSuccessAt)
		require.True(t, snapshot.LastFailureAt.IsZero())
	}

	require.NoError(t, store.Report(ctx, 42, "gpt-5.6", true, &secondTTFT, secondObservedAt, ttl))
	require.NoError(t, store.Report(ctx, 42, "gpt-5.6", false, nil, failedAt, ttl))
	account, model, err = store.Load(ctx, 42, "GPT-5.6")
	require.NoError(t, err)
	for _, snapshot := range []service.OpenAIAccountRuntimeStatSnapshot{account, model} {
		require.InDelta(t, 0.2, snapshot.ErrorRateEWMA, 1e-12)
		require.InDelta(t, 1400, snapshot.TTFTEWMA, 1e-12)
		require.Equal(t, int64(2), snapshot.SampleCount)
		require.Equal(t, secondObservedAt, snapshot.TTFTUpdatedAt)
		require.Equal(t, failedAt, snapshot.LastFailureAt)
		require.Equal(t, secondObservedAt, snapshot.LastSuccessAt)
	}
}

func TestOpenAIAccountRuntimeStatsStoreTimestampsUseAtomicMax(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	latest := time.Date(2026, time.August, 15, 2, 0, 0, 0, time.UTC)
	older := latest.Add(-time.Minute)
	ttft := 900

	require.NoError(t, store.Report(ctx, 43, "gpt-5.6", true, &ttft, latest, time.Minute))
	require.NoError(t, store.Report(ctx, 43, "gpt-5.6", true, &ttft, older, time.Minute))
	require.NoError(t, store.Report(ctx, 43, "gpt-5.6", false, nil, latest, time.Minute))
	require.NoError(t, store.Report(ctx, 43, "gpt-5.6", false, nil, older, time.Minute))

	account, model, err := store.Load(ctx, 43, "gpt-5.6")
	require.NoError(t, err)
	for _, snapshot := range []service.OpenAIAccountRuntimeStatSnapshot{account, model} {
		require.Equal(t, latest, snapshot.TTFTUpdatedAt)
		require.Equal(t, latest, snapshot.LastSuccessAt)
		require.Equal(t, latest, snapshot.LastFailureAt)
		require.Equal(t, int64(1), snapshot.SampleCount)
		require.InDelta(t, 0.2, snapshot.ErrorRateEWMA, 1e-12)
	}
}

func TestOpenAIAccountRuntimeStatsStoreNewerSuccessFollowsFailureAtMicrosecondResolution(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	failedAt := time.Date(2026, time.August, 15, 2, 15, 0, 123_456_000, time.UTC)
	succeededAt := failedAt.Add(time.Microsecond)
	ttft := 850

	require.NoError(t, store.Report(ctx, 429, "gpt-5.6", false, nil, failedAt, time.Minute))
	require.NoError(t, store.Report(ctx, 429, "gpt-5.6", true, &ttft, succeededAt, time.Minute))

	account, model, err := store.Load(ctx, 429, "gpt-5.6")
	require.NoError(t, err)
	for _, snapshot := range []service.OpenAIAccountRuntimeStatSnapshot{account, model} {
		require.Equal(t, failedAt, snapshot.LastFailureAt)
		require.Equal(t, succeededAt, snapshot.LastSuccessAt)
		require.True(t, snapshot.LastSuccessAt.After(snapshot.LastFailureAt))
		require.InDelta(t, 0.16, snapshot.ErrorRateEWMA, 1e-12)
	}
}

func TestOpenAIAccountRuntimeStatsStoreAcceptsUnixEpochObservation(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ttft := 600
	require.NoError(t, store.Report(context.Background(), 430, "gpt-5.6", true, &ttft, time.Unix(0, 0).UTC(), time.Minute))
	account, model, err := store.Load(context.Background(), 430, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, int64(1), account.SampleCount)
	require.Equal(t, time.Unix(0, 0).UTC(), account.TTFTUpdatedAt)
	require.Equal(t, time.Unix(0, 0).UTC(), account.LastSuccessAt)
	require.Equal(t, account, model)
}

func TestOpenAIAccountRuntimeStatsStoreFailureTTFTIsAccountOnly(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	ttft := 1700
	failedAt := time.Date(2026, time.August, 15, 2, 30, 0, 123_456_000, time.UTC)
	require.NoError(t, store.Report(ctx, 431, "gpt-5.6", false, &ttft, failedAt, time.Minute))

	account, model, err := store.Load(ctx, 431, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, int64(1), account.SampleCount)
	require.Equal(t, float64(ttft), account.TTFTEWMA)
	require.Equal(t, failedAt, account.TTFTUpdatedAt)
	require.Zero(t, model.SampleCount)
	require.Zero(t, model.TTFTEWMA)
	require.True(t, model.TTFTUpdatedAt.IsZero())
	require.Equal(t, failedAt, model.LastFailureAt)
}

func TestOpenAIAccountRuntimeStatsStoreConcurrentReportsAreAtomic(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	const reportCount = 64
	ctx := context.Background()
	observedAt := time.Date(2026, time.August, 15, 3, 0, 0, 0, time.UTC)
	ttft := 1250
	errs := make(chan error, reportCount)
	var wg sync.WaitGroup

	for i := 0; i < reportCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Report(ctx, 44, "gpt-5.6", true, &ttft, observedAt, time.Minute)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	account, model, err := store.Load(ctx, 44, "gpt-5.6")
	require.NoError(t, err)
	for _, snapshot := range []service.OpenAIAccountRuntimeStatSnapshot{account, model} {
		require.Equal(t, int64(reportCount), snapshot.SampleCount)
		require.Equal(t, float64(ttft), snapshot.TTFTEWMA)
	}
}

func TestOpenAIAccountRuntimeStatsStoreReportReplayIsIdempotent(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	ttft := 750
	observedAt := time.Date(2026, time.August, 15, 4, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		require.NoError(t, store.reportOpenAIAccountRuntimeStats(
			ctx, 45, "gpt-5.6", true, &ttft, observedAt, time.Minute, "same-report-id",
		))
	}
	account, model, err := store.Load(ctx, 45, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, int64(1), account.SampleCount)
	require.Equal(t, int64(1), model.SampleCount)
}

func TestOpenAIAccountRuntimeStatsStoreTTL(t *testing.T) {
	store, mr := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	ttl := 45 * time.Second
	ttft := 800
	require.NoError(t, store.Report(ctx, 46, "gpt-5.6", true, &ttft, time.Now(), ttl))
	accountKey, modelKey, dedupeKey := openAIAccountRuntimeStatsKeys(46, "gpt-5.6")
	for _, key := range []string{accountKey, modelKey, dedupeKey} {
		require.Greater(t, mr.TTL(key), time.Duration(0), key)
	}
	require.LessOrEqual(t, mr.TTL(accountKey), ttl)
	require.LessOrEqual(t, mr.TTL(modelKey), ttl)
	require.LessOrEqual(t, mr.TTL(dedupeKey), openAIAccountRuntimeReportDedupeTTL)

	mr.FastForward(ttl + time.Millisecond)
	account, model, err := store.Load(ctx, 46, "gpt-5.6")
	require.NoError(t, err)
	require.Equal(t, service.OpenAIAccountRuntimeStatSnapshot{}, account)
	require.Equal(t, service.OpenAIAccountRuntimeStatSnapshot{}, model)
	require.True(t, mr.Exists(dedupeKey))
	mr.FastForward(openAIAccountRuntimeReportDedupeTTL - ttl)
	require.False(t, mr.Exists(dedupeKey))
}

func TestOpenAIAccountRuntimeStatsStoreDedupeIsBounded(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	_, _, dedupeKey := openAIAccountRuntimeStatsKeys(461, "gpt-5.6")
	nowMillis := time.Now().UnixMilli()
	entries := make([]redis.Z, 0, openAIAccountRuntimeReportDedupeMaxItems+2)
	for i := int64(0); i < openAIAccountRuntimeReportDedupeMaxItems+2; i++ {
		entries = append(entries, redis.Z{Score: float64(nowMillis), Member: fmt.Sprintf("seed-%d", i)})
	}
	require.NoError(t, store.rdb.ZAdd(ctx, dedupeKey, entries...).Err())
	require.NoError(t, store.reportOpenAIAccountRuntimeStats(
		ctx, 461, "gpt-5.6", true, nil, time.Now(), time.Minute, "new-report",
	))
	count, err := store.rdb.ZCard(ctx, dedupeKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, count, openAIAccountRuntimeReportDedupeMaxItems)
}

func TestOpenAIAccountRuntimeStatsStoreRejectsInvalidArguments(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	testCases := []struct {
		name      string
		accountID int64
		model     string
		ttl       time.Duration
	}{
		{name: "account", accountID: 0, model: "gpt-5.6", ttl: time.Minute},
		{name: "model", accountID: 1, model: "  ", ttl: time.Minute},
		{name: "oversized model", accountID: 1, model: strings.Repeat("x", 513), ttl: time.Minute},
		{name: "ttl", accountID: 1, model: "gpt-5.6", ttl: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Error(t, store.Report(ctx, testCase.accountID, testCase.model, true, nil, time.Now(), testCase.ttl))
			_, _, err := store.Load(ctx, testCase.accountID, testCase.model)
			if testCase.name != "ttl" {
				require.Error(t, err)
			}
		})
	}

	var nilStore *gatewayCache
	require.Error(t, nilStore.Report(ctx, 1, "gpt-5.6", true, nil, time.Now(), time.Minute))
	_, _, err := nilStore.Load(ctx, 1, "gpt-5.6")
	require.Error(t, err)
}

func TestOpenAIAccountRuntimeStatsStoreLoadRejectsCorruption(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	accountKey, _, _ := openAIAccountRuntimeStatsKeys(47, "gpt-5.6")

	for _, corruptValue := range []string{"not-a-number", "NaN", "+Inf", "-1"} {
		require.NoError(t, store.rdb.Del(ctx, accountKey).Err())
		require.NoError(t, store.rdb.HSet(ctx, accountKey, "error_rate_ewma", corruptValue).Err())
		_, _, err := store.Load(ctx, 47, "gpt-5.6")
		require.Error(t, err, corruptValue)
	}
	require.NoError(t, store.rdb.Del(ctx, accountKey).Err())
	require.NoError(t, store.rdb.HSet(ctx, accountKey, "unexpected", "1").Err())
	_, _, err := store.Load(ctx, 47, "gpt-5.6")
	require.Error(t, err)
}

func TestOpenAIAccountRuntimeStatsStoreReportValidatesBeforeWriting(t *testing.T) {
	store, _ := newOpenAIAccountRuntimeStatsTestStore(t)
	ctx := context.Background()
	ttft := 1000
	observedAt := time.Date(2026, time.August, 15, 5, 0, 0, 0, time.UTC)
	require.NoError(t, store.Report(ctx, 48, "gpt-5.6", true, &ttft, observedAt, time.Minute))
	accountKey, modelKey, _ := openAIAccountRuntimeStatsKeys(48, "gpt-5.6")
	accountBefore, err := store.rdb.HGetAll(ctx, accountKey).Result()
	require.NoError(t, err)
	require.NoError(t, store.rdb.HSet(ctx, modelKey, "ttft_ewma", "corrupt").Err())

	err = store.Report(ctx, 48, "gpt-5.6", false, nil, observedAt.Add(time.Second), time.Minute)
	require.Error(t, err)
	accountAfter, err := store.rdb.HGetAll(ctx, accountKey).Result()
	require.NoError(t, err)
	require.Equal(t, accountBefore, accountAfter)
}

func TestOpenAIAccountRuntimeStatsKeysShareClusterHashTag(t *testing.T) {
	accountKey, modelKey, dedupeKey := openAIAccountRuntimeStatsKeys(49, "gpt-5.6")
	for _, key := range []string{accountKey, modelKey, dedupeKey} {
		require.Contains(t, key, "{49}")
	}
	require.NotContains(t, modelKey, "gpt-5.6")
	require.Len(t, strings.TrimPrefix(modelKey, fmt.Sprintf("%s{%d}:model:", openAIAccountRuntimeStatsPrefix, 49)), 64)
}
