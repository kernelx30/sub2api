package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupUpstreamBillingProbeRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetUpstreamBillingProbeService(service.NewUpstreamBillingProbeService(nil, nil, nil))

	router := gin.New()
	router.GET("/admin/accounts/upstream-billing-probe/settings", handler.GetUpstreamBillingProbeSettings)
	router.GET("/admin/accounts/pool-auto-priority/settings", handler.GetPoolAutoPrioritySettings)
	router.POST("/admin/accounts/upstream-billing-probe/batch", handler.ProbeUpstreamBillingBatch)
	router.PUT("/admin/accounts/:id/upstream-billing-probe", handler.SetUpstreamBillingProbeEnabled)
	router.PUT("/admin/accounts/:id/pool-auto-priority", handler.SetPoolAutoPriorityEnabled)
	return router
}

func TestAccountHandlerGetUpstreamBillingProbeSettingsReturnsDefaults(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/upstream-billing-probe/settings", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data service.UpstreamBillingProbeSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.Equal(t, 5, response.Data.IntervalMinutes)
}

func TestAccountHandlerGetPoolAutoPrioritySettingsReturnsIndependentDefaults(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/pool-auto-priority/settings", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data service.PoolAutoPrioritySettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.Equal(t, 5, response.Data.IntervalMinutes)
}

func TestAccountHandlerProbeUpstreamBillingBatchValidatesIDs(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()

	for _, body := range []string{`{"account_ids":[]}`, `{"account_ids":[0]}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/admin/accounts/upstream-billing-probe/batch", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRejectsInvalidID(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/not-an-id/upstream-billing-probe", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRequiresValue(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/1/upstream-billing-probe", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestAccountHandlerSetPoolAutoPriorityEnabledValidatesInput(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()

	for _, tc := range []struct {
		path string
		body string
	}{
		{path: "/admin/accounts/not-an-id/pool-auto-priority", body: `{"enabled":true}`},
		{path: "/admin/accounts/1/pool-auto-priority", body: `{}`},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, tc.path, bytes.NewBufferString(tc.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}

func TestAccountHandlerGetPoolAutoPriorityRankingUsesPersistedProbeData(t *testing.T) {
	now := time.Now().UTC()
	freshUntil := now.Add(10 * time.Minute)
	lastAttempt := now.Add(-time.Minute)
	nextAttempt := now.Add(4 * time.Minute)
	groupID := int64(7)
	group := &service.Group{ID: groupID, Name: "Plus", Platform: service.PlatformOpenAI}
	makeAccount := func(id int64, name string, schedulable bool, latency int64) service.Account {
		return service.Account{
			ID:          id,
			Name:        name,
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Priority:    int(id),
			Status:      service.StatusActive,
			Schedulable: schedulable,
			Credentials: map[string]any{"pool_mode": true},
			Groups:      []*service.Group{group},
			Extra: map[string]any{
				service.PoolAutoPriorityEnabledExtraKey: true,
				"quota_limit":                           100.0,
				"quota_used":                            25.0,
				service.UpstreamBillingProbeExtraKey: service.UpstreamBillingProbeSnapshot{
					Status:                  service.UpstreamBillingProbeStatusOK,
					ModelProbeStatus:        service.UpstreamBillingProbeStatusOK,
					ModelProbeModel:         "gpt-test",
					ModelProbeLatencyMS:     latency,
					ModelProbeLastAttemptAt: &lastAttempt,
					ModelProbeFreshUntil:    &freshUntil,
					ModelProbeNextAt:        &nextAttempt,
					ModelProbeHistory: []service.UpstreamModelProbeSample{
						{Status: service.UpstreamBillingProbeStatusOK, LatencyMS: latency, AttemptedAt: now.Add(-3 * time.Minute)},
						{Status: service.UpstreamBillingProbeStatusOK, LatencyMS: latency, AttemptedAt: now.Add(-2 * time.Minute)},
						{Status: service.UpstreamBillingProbeStatusOK, LatencyMS: latency, AttemptedAt: lastAttempt},
					},
				},
			},
		}
	}
	adminService := &stubAdminService{
		accountSchedulerScoreFilterAccounts: []service.Account{
			makeAccount(2, "slow", false, 2500),
			makeAccount(1, "fast", true, 1000),
		},
	}
	handler := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetUpstreamBillingProbeService(service.NewUpstreamBillingProbeService(nil, nil, nil))
	router := gin.New()
	router.GET("/admin/accounts/pool-auto-priority/ranking", handler.GetPoolAutoPriorityRanking)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/pool-auto-priority/ranking?group_id=7&limit=10", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Data poolAutoPriorityRankingResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Data.Enabled)
	require.Equal(t, 5, payload.Data.IntervalMinutes)
	require.Equal(t, groupID, payload.Data.GroupID)
	require.Equal(t, 2, payload.Data.Total)
	require.Len(t, payload.Data.Items, 2)
	require.Equal(t, int64(1), payload.Data.Items[0].AccountID)
	require.NotNil(t, payload.Data.Items[0].Rank)
	require.Equal(t, 1, *payload.Data.Items[0].Rank)
	require.Equal(t, "Plus", payload.Data.Items[0].GroupName)
	require.NotNil(t, payload.Data.Items[0].AvailableBalance)
	require.InDelta(t, 75.0, *payload.Data.Items[0].AvailableBalance, 1e-9)
	require.Nil(t, payload.Data.Items[1].Rank)
	require.False(t, payload.Data.Items[1].Schedulable)
	require.Equal(t, 1, adminService.schedulerScoreFilterCalls)
}

func TestAccountHandlerGetPoolAutoPriorityRankingValidatesQuery(t *testing.T) {
	adminService := &stubAdminService{}
	handler := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetUpstreamBillingProbeService(service.NewUpstreamBillingProbeService(nil, nil, nil))
	router := gin.New()
	router.GET("/admin/accounts/pool-auto-priority/ranking", handler.GetPoolAutoPriorityRanking)

	for _, target := range []string{
		"/admin/accounts/pool-auto-priority/ranking",
		"/admin/accounts/pool-auto-priority/ranking?group_id=bad",
		"/admin/accounts/pool-auto-priority/ranking?group_id=7&limit=51",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}
