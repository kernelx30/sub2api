package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"pool_mode":     true,
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.Equal(t, true, filtered["pool_mode"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status":                      "ok",
		"model_probe_status":          "ok",
		"model_probe_model":           "gpt-5.6-sol",
		"model_probe_endpoint":        "responses",
		"model_probe_latency_ms":      int64(1430),
		"model_probe_http_status":     200,
		"model_probe_last_error":      "",
		"model_probe_last_attempt_at": "2026-07-29T10:00:00Z",
		"model_probe_fresh_until":     "2026-07-29T10:10:00Z",
		"model_probe_next_at":         "2026-07-29T10:05:00Z",
		"model_probe_failure_count":   0,
		"model_probe_history": []any{
			map[string]any{"status": "failed", "http_status": 403, "error_type": "http_error", "attempted_at": "2026-07-29T09:55:00Z"},
			map[string]any{"status": "ok", "latency_ms": 1430, "http_status": 200, "attempted_at": "2026-07-29T10:00:00Z"},
		},
		"model_probe_sample_count":   2,
		"model_probe_success_count":  1,
		"model_probe_success_rate":   0.5,
		"model_probe_p50_latency_ms": int64(1430),
		"model_probe_p95_latency_ms": int64(1430),
		"model_probe_consecutive_ok": 1,
		"model_probe_consecutive_ng": 0,
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID:       42,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
		Extra: map[string]any{
			service.UpstreamBillingProbeExtraKey:    probe,
			service.PoolAutoPriorityEnabledExtraKey: true,
			"unused_large_field":                    "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.Equal(t, "ok", filtered["model_probe_status"])
	require.Equal(t, "gpt-5.6-sol", filtered["model_probe_model"])
	require.Equal(t, "responses", filtered["model_probe_endpoint"])
	require.Equal(t, int64(1430), filtered["model_probe_latency_ms"])
	require.Equal(t, 200, filtered["model_probe_http_status"])
	require.Equal(t, "", filtered["model_probe_last_error"])
	require.Equal(t, "2026-07-29T10:00:00Z", filtered["model_probe_last_attempt_at"])
	require.Equal(t, "2026-07-29T10:10:00Z", filtered["model_probe_fresh_until"])
	require.Equal(t, "2026-07-29T10:05:00Z", filtered["model_probe_next_at"])
	require.Equal(t, 0, filtered["model_probe_failure_count"])
	require.Len(t, filtered["model_probe_history"], 2)
	require.Equal(t, 2, filtered["model_probe_sample_count"])
	require.Equal(t, 1, filtered["model_probe_success_count"])
	require.Equal(t, 0.5, filtered["model_probe_success_rate"])
	require.Equal(t, int64(1430), filtered["model_probe_p50_latency_ms"])
	require.Equal(t, int64(1430), filtered["model_probe_p95_latency_ms"])
	require.Equal(t, 1, filtered["model_probe_consecutive_ok"])
	require.Equal(t, 0, filtered["model_probe_consecutive_ng"])
	require.Equal(t, true, metadata.Extra[service.PoolAutoPriorityEnabledExtraKey])
	require.True(t, metadata.IsPoolMode())
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), `"last_error":`)
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}
