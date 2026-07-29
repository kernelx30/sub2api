package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func openAIPoolProbeTestAccount(id int64, manualPriority int, status string, latencyMS int64) Account {
	now := time.Now()
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Priority:    manualPriority,
		Credentials: map[string]any{
			"pool_mode": true,
		},
		Extra: map[string]any{
			PoolAutoPriorityEnabledExtraKey: true,
			UpstreamBillingProbeExtraKey: map[string]any{
				"status":                      status,
				"model_probe_status":          status,
				"model_probe_model":           "gpt-5.6-sol",
				"model_probe_latency_ms":      latencyMS,
				"model_probe_last_attempt_at": now,
				"model_probe_fresh_until":     now.Add(10 * time.Minute),
			},
		},
	}
}

func TestOpenAIGatewayLoadAwareSelectionUsesPoolProbePriority(t *testing.T) {
	slowManualPrimary := openAIPoolProbeTestAccount(7101, 0, UpstreamBillingProbeStatusOK, 4200)
	fastManualBackup := openAIPoolProbeTestAccount(7102, 20, UpstreamBillingProbeStatusOK, 1400)
	accounts := []Account{slowManualPrimary, fastManualBackup}

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				slowManualPrimary.ID: {AccountID: slowManualPrimary.ID, LoadRate: 0},
				fastManualBackup.ID:  {AccountID: fastManualBackup.ID, LoadRate: 0},
			},
		}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, fastManualBackup.ID, selection.Account.ID)
	selection.ReleaseFunc()
}

func TestOpenAIAdvancedSchedulerUsesPoolProbePriorityBeforeScore(t *testing.T) {
	slowManualPrimary := openAIPoolProbeTestAccount(7201, 0, UpstreamBillingProbeStatusOK, 3800)
	fastManualBackup := openAIPoolProbeTestAccount(7202, 50, UpstreamBillingProbeStatusOK, 1200)

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 100
	svc := &OpenAIGatewayService{cfg: cfg}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	plan := scheduler.buildOpenAIAccountLoadPlan(
		context.Background(),
		OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"},
		[]*Account{&slowManualPrimary, &fastManualBackup},
		map[int64]*AccountLoadInfo{
			slowManualPrimary.ID: {AccountID: slowManualPrimary.ID},
			fastManualBackup.ID:  {AccountID: fastManualBackup.ID},
		},
	)

	require.Len(t, plan.selectionOrder, 2)
	require.Equal(t, fastManualBackup.ID, plan.selectionOrder[0].account.ID)
	require.True(t, plan.selectionOrder[0].probeRanked)
	require.Less(t, plan.selectionOrder[0].probeRank, plan.selectionOrder[1].probeRank)
}

func TestOpenAIAdvancedSchedulerRanksFailedProbeAfterHealthyProbe(t *testing.T) {
	failedManualPrimary := openAIPoolProbeTestAccount(7301, 0, UpstreamBillingProbeStatusFailed, 10000)
	healthyManualBackup := openAIPoolProbeTestAccount(7302, 20, UpstreamBillingProbeStatusOK, 1600)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}
	plan := scheduler.buildOpenAIAccountLoadPlan(
		context.Background(),
		OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"},
		[]*Account{&failedManualPrimary, &healthyManualBackup},
		map[int64]*AccountLoadInfo{},
	)

	require.Len(t, plan.selectionOrder, 2)
	require.Equal(t, healthyManualBackup.ID, plan.selectionOrder[0].account.ID)
	require.Equal(t, failedManualPrimary.ID, plan.selectionOrder[1].account.ID)
}
