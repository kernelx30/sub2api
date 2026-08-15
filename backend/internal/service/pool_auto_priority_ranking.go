package service

import (
	"math"
	"sort"
	"strings"
	"time"
)

const (
	PoolAutoPriorityProbeStatusOK                     = "ok"
	PoolAutoPriorityProbeStatusStale                  = "stale"
	PoolAutoPriorityProbeStatusUnmeasured             = "unmeasured"
	PoolAutoPriorityBalanceSourceUpstream             = "upstream_api_key_quota"
	PoolAutoPriorityBalanceSourceUpstreamWallet       = "upstream_wallet"
	PoolAutoPriorityBalanceSourceUpstreamSubscription = "upstream_subscription"
	PoolAutoPriorityRankingSourceProbe                = "probe"
	PoolAutoPriorityRankingSourceRealTraffic          = "real_traffic"
)

// PoolAutoPriorityRankingSnapshot is a read-only projection of the same probe
// cohorts used by the OpenAI runtime scheduler.
type PoolAutoPriorityRankingSnapshot struct {
	Rank                int        `json:"rank"`
	CohortRank          int        `json:"cohort_rank"`
	AccountID           int64      `json:"account_id"`
	ManualPriority      int        `json:"manual_priority"`
	ProbeStatus         string     `json:"probe_status"`
	ProbeModel          string     `json:"probe_model,omitempty"`
	SampleCount         int        `json:"sample_count"`
	SuccessRate         float64    `json:"success_rate"`
	P50LatencyMS        int64      `json:"p50_latency_ms"`
	P95LatencyMS        int64      `json:"p95_latency_ms"`
	LatestLatencyMS     int64      `json:"latest_latency_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastProbeAt         *time.Time `json:"last_probe_at,omitempty"`
	NextProbeAt         *time.Time `json:"next_probe_at,omitempty"`
	AvailableBalance    *float64   `json:"available_balance,omitempty"`
	BalanceUnlimited    bool       `json:"balance_unlimited"`
	BalanceSource       string     `json:"balance_source,omitempty"`
	RuntimeTTFTMS       float64    `json:"runtime_ttft_ms"`
	RuntimeSampleCount  int64      `json:"runtime_sample_count"`
	RuntimeUpdatedAt    *time.Time `json:"runtime_updated_at,omitempty"`
	RuntimeMature       bool       `json:"runtime_mature"`
	RankingSource       string     `json:"ranking_source"`
}

// IsPoolAutoPriorityEnabled reports the account-level opt-in exactly as the
// probe runner and scheduler do. Pool-mode accounts default to enabled.
func IsPoolAutoPriorityEnabled(account *Account) bool {
	return poolAutoPriorityEnabled(account)
}

// BuildPoolAutoPriorityRanking converts runtime probe cohorts into a stable,
// deterministic leaderboard. Probe cohort wins first, then the scheduler's
// manual account priority, then account ID as a presentation-only tie-breaker.
func BuildPoolAutoPriorityRanking(accounts []*Account, now time.Time) []PoolAutoPriorityRankingSnapshot {
	if len(accounts) == 0 {
		return []PoolAutoPriorityRankingSnapshot{}
	}

	probeRanks, ranked := probeAutoPriorityRanks(accounts, now)
	result := make([]PoolAutoPriorityRankingSnapshot, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || !IsPoolAutoPriorityEnabled(account) {
			continue
		}

		cohortRank := 0
		if ranked {
			cohortRank = probeRanks[account.ID]
		}
		result = append(result, buildPoolAutoPriorityRankingSnapshot(account, cohortRank, now))
	}

	sort.SliceStable(result, func(i, j int) bool {
		if result[i].CohortRank != result[j].CohortRank {
			return result[i].CohortRank < result[j].CohortRank
		}
		if result[i].ManualPriority != result[j].ManualPriority {
			return result[i].ManualPriority < result[j].ManualPriority
		}
		return result[i].AccountID < result[j].AccountID
	})
	for i := range result {
		result[i].Rank = i + 1
		result[i].CohortRank++
	}
	return result
}

// BuildPoolAutoPriorityRanking returns the exact effective order consumed by
// OpenAI scheduling, including mature production TTFT feedback.
func (s *OpenAIGatewayService) BuildPoolAutoPriorityRanking(accounts []*Account, now time.Time) []PoolAutoPriorityRankingSnapshot {
	result := BuildPoolAutoPriorityRanking(accounts, now)
	if len(result) == 0 || s == nil {
		return result
	}

	ranks, states, _, ranked := s.openAIEffectiveAutoPriorityRanking(accounts, now)
	for i := range result {
		state := states[result[i].AccountID]
		result[i].RankingSource = PoolAutoPriorityRankingSourceProbe
		if state.UsesRuntime || state.RecentFailure {
			result[i].RankingSource = PoolAutoPriorityRankingSourceRealTraffic
		}
		if state.UsesRuntime {
			result[i].RuntimeTTFTMS = state.TTFTMS
			result[i].RuntimeSampleCount = state.SampleCount
			result[i].RuntimeMature = state.Mature
			updatedAt := state.TTFTUpdatedAt.UTC()
			result[i].RuntimeUpdatedAt = &updatedAt
		}
	}
	if !ranked {
		return result
	}

	sort.SliceStable(result, func(i, j int) bool {
		return ranks[result[i].AccountID] < ranks[result[j].AccountID]
	})
	for i := range result {
		result[i].Rank = i + 1
	}
	return result
}

func buildPoolAutoPriorityRankingSnapshot(account *Account, cohortRank int, now time.Time) PoolAutoPriorityRankingSnapshot {
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	metrics := upstreamModelProbeWindowMetrics(snapshot, now)
	item := PoolAutoPriorityRankingSnapshot{
		CohortRank:          cohortRank,
		AccountID:           account.ID,
		ManualPriority:      account.Priority,
		ProbeStatus:         PoolAutoPriorityProbeStatusUnmeasured,
		SampleCount:         metrics.SampleCount,
		SuccessRate:         metrics.SuccessRate,
		P50LatencyMS:        metrics.P50LatencyMS,
		P95LatencyMS:        metrics.P95LatencyMS,
		ConsecutiveFailures: metrics.ConsecutiveFailures,
		RankingSource:       PoolAutoPriorityRankingSourceProbe,
	}
	item.AvailableBalance, item.BalanceUnlimited, item.BalanceSource = poolAutoPriorityAvailableBalance(account, snapshot, now)

	if snapshot == nil {
		return item
	}
	item.ProbeModel = snapshot.ModelProbeModel
	item.LatestLatencyMS = snapshot.ModelProbeLatencyMS
	if item.LatestLatencyMS <= 0 {
		item.LatestLatencyMS = snapshot.LatencyMS
	}
	item.LastProbeAt = cloneProbeTimePtr(snapshot.ModelProbeLastAttemptAt)
	item.NextProbeAt = cloneProbeTimePtr(snapshot.ModelProbeNextAt)

	switch snapshot.ModelProbeStatus {
	case UpstreamBillingProbeStatusOK:
		if isModelProbeSnapshotFresh(snapshot, now) {
			item.ProbeStatus = PoolAutoPriorityProbeStatusOK
		} else {
			item.ProbeStatus = PoolAutoPriorityProbeStatusStale
		}
	case UpstreamBillingProbeStatusFailed, UpstreamBillingProbeStatusUnsupported:
		item.ProbeStatus = snapshot.ModelProbeStatus
	}
	return item
}

func poolAutoPriorityAvailableBalance(_ *Account, snapshot *UpstreamBillingProbeSnapshot, now time.Time) (*float64, bool, string) {
	if snapshot == nil || snapshot.Data == nil {
		return nil, false, ""
	}
	balanceFreshUntil := snapshot.FreshUntil
	if rawFreshUntil, exists := snapshot.Data[upstreamBalanceFreshUntilKey]; exists {
		rawFreshUntilString, ok := rawFreshUntil.(string)
		if !ok || strings.TrimSpace(rawFreshUntilString) == "" {
			return nil, false, ""
		}
		freshUntil, err := time.Parse(time.RFC3339Nano, rawFreshUntilString)
		if err != nil {
			return nil, false, ""
		}
		balanceFreshUntil = &freshUntil
	}
	if balanceFreshUntil == nil || !now.Before(*balanceFreshUntil) {
		return nil, false, ""
	}

	source, _ := snapshot.Data["available_balance_source"].(string)
	var rankingSource string
	switch strings.TrimSpace(source) {
	case UpstreamBalanceSourceAPIKeyQuota:
		rankingSource = PoolAutoPriorityBalanceSourceUpstream
	case UpstreamBalanceSourceWallet:
		rankingSource = PoolAutoPriorityBalanceSourceUpstreamWallet
	case UpstreamBalanceSourceSubscription:
		rankingSource = PoolAutoPriorityBalanceSourceUpstreamSubscription
	default:
		return nil, false, ""
	}
	if unlimited, ok := snapshot.Data["available_balance_unlimited"].(bool); ok && unlimited {
		return nil, true, rankingSource
	}
	balance, ok := resolveAccountExtraNumber(snapshot.Data, "available_balance")
	if !ok || math.IsNaN(balance) || math.IsInf(balance, 0) ||
		(source != UpstreamBalanceSourceWallet && balance < 0) {
		return nil, false, ""
	}
	value := balance
	return &value, false, rankingSource
}
