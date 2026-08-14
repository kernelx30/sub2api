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

func TestBuildPoolAutoPriorityRankingResolvesUpstreamAndLocalBalances(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	upstream := poolRankingTestAccount(10, 0, UpstreamBillingProbeStatusOK, []int64{1000, 1000, 1000}, now)
	snapshot := decodeUpstreamBillingProbeSnapshot(upstream.Extra)
	snapshot.Data = map[string]any{
		"available_balance":        42.5,
		"available_balance_source": "api_key_quota",
	}
	upstream.Extra[UpstreamBillingProbeExtraKey] = snapshot

	local := poolRankingTestAccount(20, 0, UpstreamBillingProbeStatusOK, []int64{1100, 1100, 1100}, now)
	local.Extra["quota_limit"] = 100.0
	local.Extra["quota_used"] = 35.25

	unlimited := poolRankingTestAccount(30, 0, UpstreamBillingProbeStatusOK, []int64{1200, 1200, 1200}, now)

	got := BuildPoolAutoPriorityRanking([]*Account{upstream, local, unlimited}, now)
	byID := make(map[int64]PoolAutoPriorityRankingSnapshot, len(got))
	for _, item := range got {
		byID[item.AccountID] = item
	}

	require.NotNil(t, byID[10].AvailableBalance)
	require.InDelta(t, 42.5, *byID[10].AvailableBalance, 1e-9)
	require.Equal(t, PoolAutoPriorityBalanceSourceUpstream, byID[10].BalanceSource)
	require.NotNil(t, byID[20].AvailableBalance)
	require.InDelta(t, 64.75, *byID[20].AvailableBalance, 1e-9)
	require.Equal(t, PoolAutoPriorityBalanceSourceLocal, byID[20].BalanceSource)
	require.True(t, byID[30].BalanceUnlimited)
	require.Equal(t, PoolAutoPriorityBalanceSourceLocal, byID[30].BalanceSource)
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
