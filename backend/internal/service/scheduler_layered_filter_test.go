//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayPoolAutoPriorityGlobalSwitch(t *testing.T) {
	repo := &upstreamBillingProbeSettingRepo{values: map[string]string{
		SettingKeyPoolAutoPrioritySettings: `{"enabled":false,"interval_minutes":5}`,
	}}
	svc := &GatewayService{settingService: NewSettingService(repo, &config.Config{})}
	require.False(t, svc.poolAutoPriorityGloballyEnabled(context.Background()))

	repo.mu.Lock()
	repo.values[SettingKeyPoolAutoPrioritySettings] = `{"enabled":true,"interval_minutes":5}`
	repo.mu.Unlock()
	svc.poolAutoPriorityCheckedAt.Store(0)
	require.True(t, svc.poolAutoPriorityGloballyEnabled(context.Background()))
}

func TestFilterByMinPriority(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		result := filterByMinPriority(nil)
		require.Empty(t, result)
	})

	t.Run("single account", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 5}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinPriority(accounts)
		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})

	t.Run("multiple accounts same priority", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 3}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, Priority: 3}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, Priority: 3}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinPriority(accounts)
		require.Len(t, result, 3)
	})

	t.Run("filters to min priority only", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 5}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, Priority: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, Priority: 3}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 4, Priority: 1}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinPriority(accounts)
		require.Len(t, result, 2)
		require.Equal(t, int64(2), result[0].account.ID)
		require.Equal(t, int64(4), result[1].account.ID)
	})
}

func TestFilterByMinLoadRate(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		result := filterByMinLoadRate(nil)
		require.Empty(t, result)
	})

	t.Run("single account", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
		}
		result := filterByMinLoadRate(accounts)
		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})

	t.Run("multiple accounts same load rate", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{LoadRate: 20}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{LoadRate: 20}},
			{account: &Account{ID: 3}, loadInfo: &AccountLoadInfo{LoadRate: 20}},
		}
		result := filterByMinLoadRate(accounts)
		require.Len(t, result, 3)
	})

	t.Run("filters to min load rate only", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{LoadRate: 80}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{LoadRate: 10}},
			{account: &Account{ID: 3}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
			{account: &Account{ID: 4}, loadInfo: &AccountLoadInfo{LoadRate: 10}},
		}
		result := filterByMinLoadRate(accounts)
		require.Len(t, result, 2)
		require.Equal(t, int64(2), result[0].account.ID)
		require.Equal(t, int64(4), result[1].account.ID)
	})

	t.Run("zero load rate", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{LoadRate: 0}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
			{account: &Account{ID: 3}, loadInfo: &AccountLoadInfo{LoadRate: 0}},
		}
		result := filterByMinLoadRate(accounts)
		require.Len(t, result, 2)
		require.Equal(t, int64(1), result[0].account.ID)
		require.Equal(t, int64(3), result[1].account.ID)
	})
}

func TestSelectByLRU(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	muchEarlier := now.Add(-2 * time.Hour)

	t.Run("empty slice", func(t *testing.T) {
		result := selectByLRU(nil, false)
		require.Nil(t, result)
	})

	t.Run("single account", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{}},
		}
		result := selectByLRU(accounts, false)
		require.NotNil(t, result)
		require.Equal(t, int64(1), result.account.ID)
	})

	t.Run("selects least recently used", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: &muchEarlier}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, LastUsedAt: &earlier}, loadInfo: &AccountLoadInfo{}},
		}
		result := selectByLRU(accounts, false)
		require.NotNil(t, result)
		require.Equal(t, int64(2), result.account.ID)
	})

	t.Run("nil LastUsedAt preferred over non-nil", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: nil}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, LastUsedAt: &earlier}, loadInfo: &AccountLoadInfo{}},
		}
		result := selectByLRU(accounts, false)
		require.NotNil(t, result)
		require.Equal(t, int64(2), result.account.ID)
	})

	t.Run("multiple nil LastUsedAt random selection", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
		}
		// 多次调用应该随机选择，验证结果都在候选范围内
		validIDs := map[int64]bool{1: true, 2: true, 3: true}
		for i := 0; i < 10; i++ {
			result := selectByLRU(accounts, false)
			require.NotNil(t, result)
			require.True(t, validIDs[result.account.ID], "selected ID should be one of the candidates")
		}
	})

	t.Run("multiple same LastUsedAt random selection", func(t *testing.T) {
		sameTime := now
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: &sameTime}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: &sameTime}, loadInfo: &AccountLoadInfo{}},
		}
		// 多次调用应该随机选择
		validIDs := map[int64]bool{1: true, 2: true}
		for i := 0; i < 10; i++ {
			result := selectByLRU(accounts, false)
			require.NotNil(t, result)
			require.True(t, validIDs[result.account.ID], "selected ID should be one of the candidates")
		}
	})

	t.Run("preferOAuth selects from OAuth accounts when multiple nil", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: nil, Type: AccountTypeOAuth}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3, LastUsedAt: nil, Type: AccountTypeOAuth}, loadInfo: &AccountLoadInfo{}},
		}
		// preferOAuth 时，应该从 OAuth 类型中选择
		oauthIDs := map[int64]bool{2: true, 3: true}
		for i := 0; i < 10; i++ {
			result := selectByLRU(accounts, true)
			require.NotNil(t, result)
			require.True(t, oauthIDs[result.account.ID], "should select from OAuth accounts")
		}
	})

	t.Run("preferOAuth falls back to all when no OAuth", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: nil, Type: "session"}, loadInfo: &AccountLoadInfo{}},
		}
		// 没有 OAuth 时，从所有候选中选择
		validIDs := map[int64]bool{1: true, 2: true}
		for i := 0; i < 10; i++ {
			result := selectByLRU(accounts, true)
			require.NotNil(t, result)
			require.True(t, validIDs[result.account.ID])
		}
	})

	t.Run("preferOAuth only affects same LastUsedAt accounts", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, LastUsedAt: &earlier, Type: "session"}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, LastUsedAt: &now, Type: AccountTypeOAuth}, loadInfo: &AccountLoadInfo{}},
		}
		result := selectByLRU(accounts, true)
		require.NotNil(t, result)
		// 有不同 LastUsedAt 时，按时间选择最早的，不受 preferOAuth 影响
		require.Equal(t, int64(1), result.account.ID)
	})
}

func TestLayeredFilterIntegration(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	muchEarlier := now.Add(-2 * time.Hour)

	t.Run("full layered selection", func(t *testing.T) {
		// 模拟真实场景：多个账号，不同优先级、负载率、最后使用时间
		accounts := []accountWithLoad{
			// 优先级 1，负载 50%
			{account: &Account{ID: 1, Priority: 1, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
			// 优先级 1，负载 20%（最低）
			{account: &Account{ID: 2, Priority: 1, LastUsedAt: &earlier}, loadInfo: &AccountLoadInfo{LoadRate: 20}},
			// 优先级 1，负载 20%（最低），更早使用
			{account: &Account{ID: 3, Priority: 1, LastUsedAt: &muchEarlier}, loadInfo: &AccountLoadInfo{LoadRate: 20}},
			// 优先级 2（较低优先）
			{account: &Account{ID: 4, Priority: 2, LastUsedAt: &muchEarlier}, loadInfo: &AccountLoadInfo{LoadRate: 0}},
		}

		// 1. 取优先级最小的集合 → ID: 1, 2, 3
		step1 := filterByMinPriority(accounts)
		require.Len(t, step1, 3)

		// 2. 取负载率最低的集合 → ID: 2, 3
		step2 := filterByMinLoadRate(step1)
		require.Len(t, step2, 2)

		// 3. LRU 选择 → ID: 3（muchEarlier 最早）
		selected := selectByLRU(step2, false)
		require.NotNil(t, selected)
		require.Equal(t, int64(3), selected.account.ID)
	})

	t.Run("all same priority and load rate", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 1, LastUsedAt: &now}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
			{account: &Account{ID: 2, Priority: 1, LastUsedAt: &earlier}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
			{account: &Account{ID: 3, Priority: 1, LastUsedAt: &muchEarlier}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
		}

		step1 := filterByMinPriority(accounts)
		require.Len(t, step1, 3)

		step2 := filterByMinLoadRate(step1)
		require.Len(t, step2, 3)

		// LRU 选择最早的
		selected := selectByLRU(step2, false)
		require.NotNil(t, selected)
		require.Equal(t, int64(3), selected.account.ID)
	})
}

func TestFilterByBestProbeAutoPriority(t *testing.T) {
	now := time.Date(2026, time.July, 29, 10, 0, 0, 0, time.UTC)

	snapshotExtra := func(status string, latencyMS int64, failureCount int, fresh bool) map[string]any {
		freshUntil := now.Add(10 * time.Minute)
		if !fresh {
			freshUntil = now.Add(-time.Minute)
		}
		return map[string]any{
			PoolAutoPriorityEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey: map[string]any{
				"status":                      status,
				"model_probe_status":          status,
				"model_probe_latency_ms":      latencyMS,
				"model_probe_last_attempt_at": now,
				"model_probe_fresh_until":     freshUntil,
				"model_probe_failure_count":   failureCount,
			},
		}
	}
	historyExtra := func(statuses []string, latencies []int64) map[string]any {
		require.Len(t, latencies, len(statuses))
		history := make([]UpstreamModelProbeSample, 0, len(statuses))
		for i, status := range statuses {
			httpStatus := http.StatusOK
			errorType := ""
			if status == UpstreamBillingProbeStatusFailed {
				httpStatus = http.StatusForbidden
				errorType = "http_error"
			} else if status == UpstreamBillingProbeStatusUnsupported {
				errorType = "responses_tool_unsupported"
			}
			history = append(history, UpstreamModelProbeSample{
				Status:      status,
				LatencyMS:   latencies[i],
				HTTPStatus:  httpStatus,
				ErrorType:   errorType,
				AttemptedAt: now.Add(-time.Duration(len(statuses)-1-i) * 5 * time.Minute),
			})
		}
		last := history[len(history)-1]
		return map[string]any{
			PoolAutoPriorityEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey: map[string]any{
				"status":                      last.Status,
				"model_probe_status":          last.Status,
				"model_probe_latency_ms":      last.LatencyMS,
				"model_probe_http_status":     last.HTTPStatus,
				"model_probe_last_error":      last.ErrorType,
				"model_probe_last_attempt_at": last.AttemptedAt,
				"model_probe_fresh_until":     now.Add(10 * time.Minute),
				"model_probe_history":         history,
			},
		}
	}
	poolAccount := func(id int64, priority int, extra map[string]any) *Account {
		return &Account{ID: id, Type: AccountTypeAPIKey, Priority: priority, Credentials: map[string]any{"pool_mode": true}, Extra: extra}
	}

	t.Run("healthy faster account wins over lower manual priority", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: poolAccount(1, 1, snapshotExtra(UpstreamBillingProbeStatusOK, 2200, 0, true)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 9, snapshotExtra(UpstreamBillingProbeStatusOK, 900, 0, true)), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(2), result[0].account.ID)
	})

	t.Run("close healthy latency keeps both candidates for manual tie breakers", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: poolAccount(1, 1, snapshotExtra(UpstreamBillingProbeStatusOK, 1000, 0, true)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 2, snapshotExtra(UpstreamBillingProbeStatusOK, 1150, 0, true)), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 2)
		require.Equal(t, int64(1), result[0].account.ID)
		require.Equal(t, int64(2), result[1].account.ID)
	})

	t.Run("failed account is ranked after unmeasured account", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: poolAccount(1, 1, snapshotExtra(UpstreamBillingProbeStatusFailed, 0, 2, true)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 5, nil), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(2), result[0].account.ID)
	})

	t.Run("model failure beats fast billing metadata", func(t *testing.T) {
		freshUntil := now.Add(10 * time.Minute)
		accounts := []accountWithLoad{
			{
				account: &Account{
					ID:          1,
					Type:        AccountTypeAPIKey,
					Priority:    1,
					Credentials: map[string]any{"pool_mode": true},
					Extra: map[string]any{
						PoolAutoPriorityEnabledExtraKey: true,
						UpstreamBillingProbeExtraKey: map[string]any{
							"status":                  UpstreamBillingProbeStatusOK,
							"latency_ms":              int64(350),
							"model_probe_status":      UpstreamBillingProbeStatusFailed,
							"model_probe_latency_ms":  int64(500),
							"model_probe_http_status": http.StatusForbidden,
							"last_attempt_at":         now,
							"fresh_until":             freshUntil,
							"failure_count":           1,
						},
					},
				},
				loadInfo: &AccountLoadInfo{},
			},
			{
				account: &Account{
					ID:          2,
					Type:        AccountTypeAPIKey,
					Priority:    9,
					Credentials: map[string]any{"pool_mode": true},
					Extra: map[string]any{
						PoolAutoPriorityEnabledExtraKey: true,
						UpstreamBillingProbeExtraKey: map[string]any{
							"status":                  UpstreamBillingProbeStatusOK,
							"latency_ms":              int64(1800),
							"model_probe_status":      UpstreamBillingProbeStatusOK,
							"model_probe_latency_ms":  int64(900),
							"model_probe_http_status": http.StatusOK,
							"last_attempt_at":         now,
							"fresh_until":             freshUntil,
						},
					},
				},
				loadInfo: &AccountLoadInfo{},
			},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(2), result[0].account.ID)
	})

	t.Run("model latency overrides billing latency", func(t *testing.T) {
		freshUntil := now.Add(10 * time.Minute)
		accounts := []accountWithLoad{
			{
				account: &Account{
					ID:          1,
					Type:        AccountTypeAPIKey,
					Priority:    1,
					Credentials: map[string]any{"pool_mode": true},
					Extra: map[string]any{
						PoolAutoPriorityEnabledExtraKey: true,
						UpstreamBillingProbeExtraKey: map[string]any{
							"status":                 UpstreamBillingProbeStatusOK,
							"latency_ms":             int64(300),
							"model_probe_status":     UpstreamBillingProbeStatusOK,
							"model_probe_latency_ms": int64(2200),
							"last_attempt_at":        now,
							"fresh_until":            freshUntil,
						},
					},
				},
				loadInfo: &AccountLoadInfo{},
			},
			{
				account: &Account{
					ID:          2,
					Type:        AccountTypeAPIKey,
					Priority:    9,
					Credentials: map[string]any{"pool_mode": true},
					Extra: map[string]any{
						PoolAutoPriorityEnabledExtraKey: true,
						UpstreamBillingProbeExtraKey: map[string]any{
							"status":                 UpstreamBillingProbeStatusOK,
							"latency_ms":             int64(1800),
							"model_probe_status":     UpstreamBillingProbeStatusOK,
							"model_probe_latency_ms": int64(900),
							"last_attempt_at":        now,
							"fresh_until":            freshUntil,
						},
					},
				},
				loadInfo: &AccountLoadInfo{},
			},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(2), result[0].account.ID)
	})

	t.Run("one lucky response does not outrank established history", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: poolAccount(1, 5, historyExtra(
				[]string{"ok", "ok", "ok", "ok", "ok", "ok"},
				[]int64{1800, 1850, 1900, 1750, 1800, 1850},
			)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 1, historyExtra(
				[]string{"ok"},
				[]int64{250},
			)), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})

	t.Run("p95 tail spike demotes an otherwise fast account", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: poolAccount(1, 5, historyExtra(
				[]string{"ok", "ok", "ok", "ok", "ok", "ok"},
				[]int64{1500, 1550, 1600, 1650, 1550, 1600},
			)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 1, historyExtra(
				[]string{"ok", "ok", "ok", "ok", "ok", "ok"},
				[]int64{600, 650, 700, 9000, 650, 700},
			)), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})

	t.Run("every account receives a rank including failures and opt outs", func(t *testing.T) {
		optingOut := snapshotExtra(UpstreamBillingProbeStatusOK, 100, 0, true)
		optingOut[PoolAutoPriorityEnabledExtraKey] = false
		accounts := []*Account{
			poolAccount(1, 1, historyExtra([]string{"ok", "ok", "ok"}, []int64{900, 950, 1000})),
			poolAccount(2, 1, nil),
			poolAccount(3, 1, historyExtra([]string{"ok", "failed"}, []int64{900, 300})),
			poolAccount(4, 1, historyExtra([]string{"unsupported"}, []int64{200})),
			poolAccount(5, 1, optingOut),
		}

		ranks, ranked := probeAutoPriorityRanks(accounts, now)

		require.True(t, ranked)
		require.Len(t, ranks, len(accounts))
		for _, account := range accounts {
			_, ok := ranks[account.ID]
			require.True(t, ok, "account %d is missing a rank", account.ID)
		}
		require.Less(t, ranks[1], ranks[2])
		require.Less(t, ranks[2], ranks[3])
		require.Less(t, ranks[3], ranks[4])
	})

	t.Run("sticky escape uses current failure rolling reliability and p95", func(t *testing.T) {
		failed := poolAccount(1, 1, historyExtra(
			[]string{"ok", "ok", "failed"},
			[]int64{900, 950, 400},
		))
		reason, _ := accountProbeStickyEscapeReason(failed, now)
		require.Equal(t, "model_probe_failed", reason)

		unreliable := poolAccount(2, 1, historyExtra(
			[]string{"failed", "ok", "failed", "ok"},
			[]int64{300, 900, 400, 950},
		))
		reason, metrics := accountProbeStickyEscapeReason(unreliable, now)
		require.Equal(t, "model_probe_success_rate", reason)
		require.InDelta(t, 0.5, metrics.SuccessRate, 1e-9)

		slowTail := poolAccount(3, 1, historyExtra(
			[]string{"ok", "ok", "ok", "ok"},
			[]int64{900, 950, 9000, 1000},
		))
		reason, metrics = accountProbeStickyEscapeReason(slowTail, now)
		require.Equal(t, "model_probe_p95", reason)
		require.Equal(t, int64(9000), metrics.P95LatencyMS)
	})

	t.Run("no probe signal keeps original set", func(t *testing.T) {
		accounts := []accountWithLoad{
			{account: &Account{ID: 1, Priority: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2, Priority: 2}, loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 2)
		require.Equal(t, int64(1), result[0].account.ID)
		require.Equal(t, int64(2), result[1].account.ID)
	})

	t.Run("explicit pool opt out is excluded from dynamic ordering", func(t *testing.T) {
		optingOut := snapshotExtra(UpstreamBillingProbeStatusOK, 100, 0, true)
		optingOut[PoolAutoPriorityEnabledExtraKey] = false
		accounts := []accountWithLoad{
			{account: poolAccount(1, 1, snapshotExtra(UpstreamBillingProbeStatusOK, 900, 0, true)), loadInfo: &AccountLoadInfo{}},
			{account: poolAccount(2, 1, optingOut), loadInfo: &AccountLoadInfo{}},
		}

		result := filterByBestProbeAutoPriority(accounts, now)

		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})
}
