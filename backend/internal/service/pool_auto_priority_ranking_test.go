package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func poolRankingTestAccount(
	id int64,
	priority int,
	status string,
	latencies []int64,
	now time.Time,
) *Account {
	history := make([]UpstreamModelProbeSample, 0, len(latencies))
	for i, latency := range latencies {
		history = append(history, UpstreamModelProbeSample{
			Status:      status,
			LatencyMS:   latency,
			AttemptedAt: now.Add(-time.Duration(len(latencies)-i) * time.Minute),
		})
	}
	freshUntil := now.Add(10 * time.Minute)
	lastAttempt := now.Add(-time.Minute)
	return &Account{
		ID:          id,
		Name:        "account",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Priority:    priority,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"pool_mode": true},
		Extra: map[string]any{
			PoolAutoPriorityEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey: UpstreamBillingProbeSnapshot{
				Status:                  UpstreamBillingProbeStatusOK,
				ModelProbeStatus:        status,
				ModelProbeModel:         "gpt-test",
				ModelProbeLatencyMS:     latencies[len(latencies)-1],
				ModelProbeLastAttemptAt: &lastAttempt,
				ModelProbeFreshUntil:    &freshUntil,
				ModelProbeHistory:       history,
			},
		},
	}
}

func TestBuildPoolAutoPriorityRankingUsesRuntimeCohortsBeforeManualPriority(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	fast := poolRankingTestAccount(10, 100, UpstreamBillingProbeStatusOK, []int64{900, 1000, 1100}, now)
	slow := poolRankingTestAccount(20, 0, UpstreamBillingProbeStatusOK, []int64{2800, 3000, 3200}, now)
	failed := poolRankingTestAccount(30, -10, UpstreamBillingProbeStatusFailed, []int64{0, 0, 0}, now)

	got := BuildPoolAutoPriorityRanking([]*Account{failed, slow, fast}, now)

	require.Len(t, got, 3)
	require.Equal(t, []int64{10, 20, 30}, []int64{got[0].AccountID, got[1].AccountID, got[2].AccountID})
	require.Equal(t, []int{1, 2, 3}, []int{got[0].Rank, got[1].Rank, got[2].Rank})
	require.Less(t, got[0].CohortRank, got[1].CohortRank)
	require.Less(t, got[1].CohortRank, got[2].CohortRank)
	require.Equal(t, int64(1000), got[0].P50LatencyMS)
	require.Equal(t, int64(1100), got[0].P95LatencyMS)
}

func TestBuildPoolAutoPriorityRankingUsesManualPriorityInsideLatencySlack(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	lowerPriority := poolRankingTestAccount(10, 0, UpstreamBillingProbeStatusOK, []int64{1000, 1050, 1100}, now)
	higherPriority := poolRankingTestAccount(20, 10, UpstreamBillingProbeStatusOK, []int64{1030, 1080, 1130}, now)

	got := BuildPoolAutoPriorityRanking([]*Account{higherPriority, lowerPriority}, now)

	require.Len(t, got, 2)
	require.Equal(t, got[0].CohortRank, got[1].CohortRank)
	require.Equal(t, int64(10), got[0].AccountID)
	require.Equal(t, int64(20), got[1].AccountID)
}

func TestProbeAutoPrioritySelectionRanksMatchDisplayedRanking(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	fastLowerManualPriority := poolRankingTestAccount(30, 10, UpstreamBillingProbeStatusOK, []int64{1000, 1050, 1100}, now)
	fastHigherManualPriority := poolRankingTestAccount(20, 0, UpstreamBillingProbeStatusOK, []int64{1030, 1080, 1130}, now)
	slow := poolRankingTestAccount(10, -10, UpstreamBillingProbeStatusOK, []int64{2800, 3000, 3200}, now)
	accounts := []*Account{slow, fastLowerManualPriority, fastHigherManualPriority}

	displayed := BuildPoolAutoPriorityRanking(accounts, now)
	selectionRanks, ranked := probeAutoPrioritySelectionRanks(accounts, now)

	require.True(t, ranked)
	require.Len(t, displayed, len(accounts))
	for i, item := range displayed {
		require.Equal(t, i, selectionRanks[item.AccountID])
	}
}

func TestBuildPoolAutoPriorityRankingOnlyShowsVerifiedUpstreamBalances(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	upstream := poolRankingTestAccount(10, 0, UpstreamBillingProbeStatusOK, []int64{1000, 1000, 1000}, now)
	snapshot := decodeUpstreamBillingProbeSnapshot(upstream.Extra)
	snapshot.Data = map[string]any{
		"available_balance":        42.5,
		"available_balance_source": "api_key_quota",
	}
	snapshot.FreshUntil = probeTimePtr(now.Add(10 * time.Minute))
	upstream.Extra[UpstreamBillingProbeExtraKey] = snapshot

	wallet := poolRankingTestAccount(20, 0, UpstreamBillingProbeStatusOK, []int64{1100, 1100, 1100}, now)
	walletSnapshot := decodeUpstreamBillingProbeSnapshot(wallet.Extra)
	walletSnapshot.Data = map[string]any{
		"available_balance":          -0.11810209,
		"available_balance_source":   UpstreamBalanceSourceWallet,
		upstreamBalanceFreshUntilKey: now.Add(10 * time.Minute).Format(time.RFC3339Nano),
	}
	wallet.Extra[UpstreamBillingProbeExtraKey] = walletSnapshot

	localOnly := poolRankingTestAccount(30, 0, UpstreamBillingProbeStatusOK, []int64{1200, 1200, 1200}, now)
	localOnly.Extra["quota_limit"] = 100.0
	localOnly.Extra["quota_used"] = 35.25

	got := BuildPoolAutoPriorityRanking([]*Account{upstream, wallet, localOnly}, now)
	byID := make(map[int64]PoolAutoPriorityRankingSnapshot, len(got))
	for _, item := range got {
		byID[item.AccountID] = item
	}

	require.NotNil(t, byID[10].AvailableBalance)
	require.InDelta(t, 42.5, *byID[10].AvailableBalance, 1e-9)
	require.Equal(t, PoolAutoPriorityBalanceSourceUpstream, byID[10].BalanceSource)
	require.NotNil(t, byID[20].AvailableBalance)
	require.InDelta(t, -0.11810209, *byID[20].AvailableBalance, 1e-9)
	require.Equal(t, PoolAutoPriorityBalanceSourceUpstreamWallet, byID[20].BalanceSource)
	require.Nil(t, byID[30].AvailableBalance)
	require.False(t, byID[30].BalanceUnlimited)
	require.Empty(t, byID[30].BalanceSource)
}

func TestParseUpstreamBillingProbeResponseKeepsOptionalAvailableBalance(t *testing.T) {
	body := []byte(`{
		"object":"sub2api.key_billing",
		"schema_version":1,
		"billing_scope":"token",
		"available_balance":23.75,
		"available_balance_source":"api_key_quota",
		"group_rate_multiplier":1,
		"resolved_rate_multiplier":1,
		"peak_rate_enabled":false,
		"effective_rate_multiplier":1,
		"observed_at":"2026-08-14T10:00:00Z"
	}`)

	data, err := parseUpstreamBillingProbeResponse(body)

	require.NoError(t, err)
	require.Equal(t, "api_key_quota", data["available_balance_source"])
	require.InDelta(t, 23.75, data["available_balance"], 1e-9)
}

func TestParseUpstreamUsageBalanceResponseSupportsRealBalanceModes(t *testing.T) {
	t.Run("wallet keeps negative real balance", func(t *testing.T) {
		result, err := parseUpstreamUsageBalanceResponse([]byte(`{
			"mode":"unrestricted",
			"isValid":true,
			"remaining":-0.11810209,
			"balance":-0.11810209,
			"unit":"USD"
		}`))

		require.NoError(t, err)
		require.Equal(t, UpstreamBalanceSourceWallet, result.Source)
		require.NotNil(t, result.AvailableBalance)
		require.InDelta(t, -0.11810209, *result.AvailableBalance, 1e-12)
	})

	t.Run("quota limited uses key remaining quota", func(t *testing.T) {
		result, err := parseUpstreamUsageBalanceResponse([]byte(`{
			"mode":"quota_limited",
			"remaining":8.25,
			"unit":"USD",
			"quota":{"remaining":8.25,"unit":"USD"}
		}`))

		require.NoError(t, err)
		require.Equal(t, UpstreamBalanceSourceAPIKeyQuota, result.Source)
		require.NotNil(t, result.AvailableBalance)
		require.InDelta(t, 8.25, *result.AvailableBalance, 1e-12)
	})

	t.Run("subscription reports unlimited", func(t *testing.T) {
		result, err := parseUpstreamUsageBalanceResponse([]byte(`{
			"mode":"unrestricted",
			"remaining":-1,
			"unit":"USD",
			"subscription":{"daily_limit_usd":null}
		}`))

		require.NoError(t, err)
		require.Equal(t, UpstreamBalanceSourceSubscription, result.Source)
		require.True(t, result.Unlimited)
		require.Nil(t, result.AvailableBalance)
	})

	t.Run("invalid key response is rejected", func(t *testing.T) {
		result, err := parseUpstreamUsageBalanceResponse([]byte(`{
			"mode":"unrestricted",
			"isValid":false,
			"balance":99,
			"unit":"USD"
		}`))

		require.ErrorContains(t, err, "invalid API key")
		require.Nil(t, result)
	})
}

func TestShouldProbeUpstreamUsageBalancePrefersLiveWalletEndpoint(t *testing.T) {
	require.True(t, shouldProbeUpstreamUsageBalance(map[string]any{
		"available_balance_source": UpstreamBalanceSourceWallet,
		"available_balance":        12.5,
	}))
	require.True(t, shouldProbeUpstreamUsageBalance(map[string]any{
		"available_balance_source":    UpstreamBalanceSourceAPIKeyQuota,
		"available_balance_unlimited": true,
	}))
	require.False(t, shouldProbeUpstreamUsageBalance(map[string]any{
		"available_balance_source": UpstreamBalanceSourceAPIKeyQuota,
		"available_balance":        8.25,
	}))
}

func TestPoolAutoPriorityRankingHidesExpiredRealBalance(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	account := poolRankingTestAccount(10, 0, UpstreamBillingProbeStatusOK, []int64{1000, 1000, 1000}, now)
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	snapshot.Data = map[string]any{
		"available_balance":          1.5,
		"available_balance_source":   UpstreamBalanceSourceWallet,
		upstreamBalanceFreshUntilKey: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	account.Extra[UpstreamBillingProbeExtraKey] = snapshot

	got := BuildPoolAutoPriorityRanking([]*Account{account}, now)

	require.Len(t, got, 1)
	require.Nil(t, got[0].AvailableBalance)
	require.False(t, got[0].BalanceUnlimited)
	require.Empty(t, got[0].BalanceSource)
}
