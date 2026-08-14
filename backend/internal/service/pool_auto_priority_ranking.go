package service

import (
	"math"
	"sort"
	"strings"
	"time"
)

const (
	PoolAutoPriorityProbeStatusOK         = "ok"
	PoolAutoPriorityProbeStatusStale      = "stale"
	PoolAutoPriorityProbeStatusUnmeasured = "unmeasured"
	PoolAutoPriorityBalanceSourceUpstream = "upstream_api_key_quota"
	PoolAutoPriorityBalanceSourceLocal    = "local_account_quota"
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
	}
	item.AvailableBalance, item.BalanceUnlimited, item.BalanceSource = poolAutoPriorityAvailableBalance(account, snapshot)

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

func poolAutoPriorityAvailableBalance(account *Account, snapshot *UpstreamBillingProbeSnapshot) (*float64, bool, string) {
	if snapshot != nil && snapshot.Data != nil {
		source, _ := snapshot.Data["available_balance_source"].(string)
		if strings.TrimSpace(source) == "api_key_quota" {
			if unlimited, ok := snapshot.Data["available_balance_unlimited"].(bool); ok && unlimited {
				return nil, true, PoolAutoPriorityBalanceSourceUpstream
			}
			if balance, ok := resolveAccountExtraNumber(snapshot.Data, "available_balance"); ok &&
				balance >= 0 && !math.IsNaN(balance) && !math.IsInf(balance, 0) {
				value := balance
				return &value, false, PoolAutoPriorityBalanceSourceUpstream
			}
		}
	}

	if account == nil || !account.IsAPIKeyOrBedrock() {
		return nil, false, ""
	}
	limit := account.GetQuotaLimit()
	if limit <= 0 {
		return nil, true, PoolAutoPriorityBalanceSourceLocal
	}
	remaining := limit - account.GetQuotaUsed()
	if remaining < 0 {
		remaining = 0
	}
	return &remaining, false, PoolAutoPriorityBalanceSourceLocal
}
