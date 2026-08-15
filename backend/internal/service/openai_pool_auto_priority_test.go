package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const openAIPoolProbeRuntimeSamples = 3

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

func TestOpenAIGatewayLoadAwareSelectionUsesRealTrafficTTFTAndMatchesDisplayedRanking(t *testing.T) {
	probeWinner := openAIPoolProbeTestAccount(7151, 0, UpstreamBillingProbeStatusOK, 1000)
	productionWinner := openAIPoolProbeTestAccount(7152, 20, UpstreamBillingProbeStatusOK, 2200)
	accounts := []Account{probeWinner, productionWinner}

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				probeWinner.ID:      {AccountID: probeWinner.ID},
				productionWinner.ID: {AccountID: productionWinner.ID},
			},
		}),
	}
	for i := 0; i < openAIPoolProbeRuntimeSamples; i++ {
		svc.ReportOpenAIAccountScheduleResult(probeWinner.ID, "gpt-5.6-sol", true, intPtrForTest(30000))
		svc.ReportOpenAIAccountScheduleResult(productionWinner.ID, "gpt-5.6-sol", true, intPtrForTest(4000))
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "gpt-5.6-sol", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, productionWinner.ID, selection.Account.ID)
	require.NotNil(t, svc.openaiAccountStats)

	ranking := svc.BuildPoolAutoPriorityRanking([]*Account{&accounts[0], &accounts[1]}, time.Now())
	require.Len(t, ranking, 2)
	require.Equal(t, selection.Account.ID, ranking[0].AccountID)
	require.Equal(t, PoolAutoPriorityRankingSourceRealTraffic, ranking[0].RankingSource)
	require.True(t, ranking[0].RuntimeMature)
	require.Equal(t, int64(openAIPoolProbeRuntimeSamples), ranking[0].RuntimeSampleCount)
	selection.ReleaseFunc()
}

func TestOpenAIGatewayLoadAwareSelectionComparesRuntimeTTFTWithProbeFallback(t *testing.T) {
	probeWinnerButRuntimeSlow := openAIPoolProbeTestAccount(7161, 0, UpstreamBillingProbeStatusOK, 1750)
	probeBackupWithoutRuntime := openAIPoolProbeTestAccount(7162, 20, UpstreamBillingProbeStatusOK, 2381)
	accounts := []Account{probeWinnerButRuntimeSlow, probeBackupWithoutRuntime}

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				probeWinnerButRuntimeSlow.ID: {AccountID: probeWinnerButRuntimeSlow.ID},
				probeBackupWithoutRuntime.ID: {AccountID: probeBackupWithoutRuntime.ID},
			},
		}),
	}
	for i := 0; i < openAIPoolProbeRuntimeSamples; i++ {
		svc.ReportOpenAIAccountScheduleResult(probeWinnerButRuntimeSlow.ID, "gpt-5.6-sol", true, intPtrForTest(8125))
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "gpt-5.6-sol", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, probeBackupWithoutRuntime.ID, selection.Account.ID)
	require.NotNil(t, svc.openaiAccountStats)

	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: svc.openaiAccountStats}
	plan := scheduler.buildOpenAIAccountLoadPlan(
		context.Background(),
		OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"},
		[]*Account{&accounts[0], &accounts[1]},
		map[int64]*AccountLoadInfo{},
	)
	require.Len(t, plan.selectionOrder, 2)
	require.Equal(t, probeBackupWithoutRuntime.ID, plan.selectionOrder[0].account.ID)

	ranking := svc.BuildPoolAutoPriorityRanking([]*Account{&accounts[0], &accounts[1]}, time.Now())
	require.Len(t, ranking, 2)
	require.Equal(t, probeBackupWithoutRuntime.ID, ranking[0].AccountID)
	require.Equal(t, PoolAutoPriorityRankingSourceRealTraffic, ranking[0].RankingSource)
	require.False(t, ranking[0].RuntimeMature)
	selection.ReleaseFunc()
}

func TestOpenAIGatewayLoadAwareSelectionMatchesDisplayedRankInsideProbeCohort(t *testing.T) {
	displayedFirst := openAIPoolProbeTestAccount(7171, 0, UpstreamBillingProbeStatusOK, 1000)
	displayedSecond := openAIPoolProbeTestAccount(7172, 0, UpstreamBillingProbeStatusOK, 1050)
	accounts := []Account{displayedSecond, displayedFirst}

	ranking := BuildPoolAutoPriorityRanking([]*Account{&accounts[0], &accounts[1]}, time.Now())
	require.Len(t, ranking, 2)
	require.Equal(t, displayedFirst.ID, ranking[0].AccountID)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				displayedFirst.ID:  {AccountID: displayedFirst.ID, LoadRate: 90},
				displayedSecond.ID: {AccountID: displayedSecond.ID, LoadRate: 0},
			},
		}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "gpt-5.6-sol", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, ranking[0].AccountID, selection.Account.ID)
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

func TestOpenAIAdvancedSchedulerUsesRealTrafficTTFT(t *testing.T) {
	probeWinner := openAIPoolProbeTestAccount(7251, 0, UpstreamBillingProbeStatusOK, 1000)
	productionWinner := openAIPoolProbeTestAccount(7252, 20, UpstreamBillingProbeStatusOK, 2200)

	stats := newOpenAIAccountRuntimeStats()
	for i := 0; i < openAIPoolProbeRuntimeSamples; i++ {
		stats.report(probeWinner.ID, true, intPtrForTest(30000))
		stats.report(productionWinner.ID, true, intPtrForTest(4000))
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, openaiAccountStats: stats}
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: stats}
	plan := scheduler.buildOpenAIAccountLoadPlan(
		context.Background(),
		OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"},
		[]*Account{&probeWinner, &productionWinner},
		map[int64]*AccountLoadInfo{},
	)

	require.Len(t, plan.selectionOrder, 2)
	require.Equal(t, productionWinner.ID, plan.selectionOrder[0].account.ID)
}

func TestOpenAIAdvancedSchedulerWaitsForStableRealTrafficTTFT(t *testing.T) {
	probeWinner := openAIPoolProbeTestAccount(7261, 0, UpstreamBillingProbeStatusOK, 1000)
	probeBackup := openAIPoolProbeTestAccount(7262, 20, UpstreamBillingProbeStatusOK, 2200)

	stats := newOpenAIAccountRuntimeStats()
	for i := int64(0); i < openAIRealTrafficTTFTMinSamples-1; i++ {
		stats.report(probeWinner.ID, true, intPtrForTest(30000))
		stats.report(probeBackup.ID, true, intPtrForTest(4000))
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, openaiAccountStats: stats}
	scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: stats}
	plan := scheduler.buildOpenAIAccountLoadPlan(
		context.Background(),
		OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"},
		[]*Account{&probeWinner, &probeBackup},
		map[int64]*AccountLoadInfo{},
	)

	require.Len(t, plan.selectionOrder, 2)
	require.Equal(t, probeWinner.ID, plan.selectionOrder[0].account.ID)
	ranking := svc.BuildPoolAutoPriorityRanking([]*Account{&probeWinner, &probeBackup}, time.Now())
	require.Equal(t, PoolAutoPriorityRankingSourceProbe, ranking[0].RankingSource)
	require.False(t, ranking[0].RuntimeMature)
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

func TestOpenAIStickyFailedProbeIsClearedAndReboundToHealthyAccount(t *testing.T) {
	ctx := context.Background()
	groupID := int64(7400)
	failedSticky := openAIPoolProbeTestAccount(7401, 0, UpstreamBillingProbeStatusFailed, 500)
	healthy := openAIPoolProbeTestAccount(7402, 20, UpstreamBillingProbeStatusOK, 1400)
	failedSticky.GroupIDs = []int64{groupID}
	healthy.GroupIDs = []int64{groupID}

	sessionHash := "pool_probe_escape"
	cacheKey := "openai:" + sessionHash
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: failedSticky.ID}}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{failedSticky, healthy}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		sessionHash,
		"gpt-5.6-sol",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, healthy.ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Positive(t, cache.deletedSessions[cacheKey])
	require.Equal(t, healthy.ID, cache.sessionBindings[cacheKey])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIStickyMixedRuntimeAndProbeTTFTIsClearedAndRebound(t *testing.T) {
	ctx := context.Background()
	groupID := int64(7450)
	probeWinner := openAIPoolProbeTestAccount(7451, 0, UpstreamBillingProbeStatusOK, 1750)
	productionWinner := openAIPoolProbeTestAccount(7452, 20, UpstreamBillingProbeStatusOK, 2381)
	probeWinner.GroupIDs = []int64{groupID}
	productionWinner.GroupIDs = []int64{groupID}

	sessionHash := "real_ttft_escape"
	cacheKey := "openai:" + sessionHash
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: probeWinner.ID}}
	stats := newOpenAIAccountRuntimeStats()
	for i := 0; i < openAIPoolProbeRuntimeSamples; i++ {
		stats.report(probeWinner.ID, true, intPtrForTest(8125))
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{probeWinner, productionWinner}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		openaiAccountStats: stats,
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		sessionHash,
		"gpt-5.6-sol",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, productionWinner.ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Positive(t, cache.deletedSessions[cacheKey])
	require.Equal(t, productionWinner.ID, cache.sessionBindings[cacheKey])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAILegacyStickySlowRealTrafficIsClearedAndRebound(t *testing.T) {
	ctx := context.Background()
	groupID := int64(7460)
	probeWinner := openAIPoolProbeTestAccount(7461, 0, UpstreamBillingProbeStatusOK, 1000)
	productionWinner := openAIPoolProbeTestAccount(7462, 20, UpstreamBillingProbeStatusOK, 2200)
	probeWinner.GroupIDs = []int64{groupID}
	productionWinner.GroupIDs = []int64{groupID}

	sessionHash := "legacy_real_ttft_escape"
	cacheKey := "openai:" + sessionHash
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{cacheKey: probeWinner.ID}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{probeWinner, productionWinner}},
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	for i := 0; i < openAIPoolProbeRuntimeSamples; i++ {
		svc.ReportOpenAIAccountScheduleResult(probeWinner.ID, "gpt-5.6-sol", true, intPtrForTest(30000))
		svc.ReportOpenAIAccountScheduleResult(productionWinner.ID, "gpt-5.6-sol", true, intPtrForTest(4000))
	}

	selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "gpt-5.6-sol", nil)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, productionWinner.ID, selection.Account.ID)
	require.Positive(t, cache.deletedSessions[cacheKey])
	require.Equal(t, productionWinner.ID, cache.sessionBindings[cacheKey])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}
