package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	openAIGroupFallbackPrimaryID  int64 = 5005
	openAIGroupFallbackTargetID   int64 = 5009
	openAIGroupFallbackPrimaryAcc int64 = 6005
	openAIGroupFallbackTargetAcc  int64 = 6009
)

type openAIGroupFallbackAccountRepo struct {
	service.AccountRepository
	accountsByGroup map[int64][]service.Account
}

func (r *openAIGroupFallbackAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]service.Account, error) {
	return filterOpenAIGroupFallbackAccounts(r.accountsByGroup[groupID], platform), nil
}

func (r *openAIGroupFallbackAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	var accounts []service.Account
	for _, groupAccounts := range r.accountsByGroup {
		accounts = append(accounts, filterOpenAIGroupFallbackAccounts(groupAccounts, platform)...)
	}
	return accounts, nil
}

func (r *openAIGroupFallbackAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return nil, nil
}

func (r *openAIGroupFallbackAccountRepo) ListModelAvailabilityCandidates(_ context.Context, groupID *int64, platforms []string, _ bool) ([]service.Account, error) {
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	var source []service.Account
	if groupID != nil {
		source = r.accountsByGroup[*groupID]
	} else {
		for _, groupAccounts := range r.accountsByGroup {
			source = append(source, groupAccounts...)
		}
	}
	out := make([]service.Account, 0, len(source))
	for _, account := range source {
		if _, ok := allowed[account.Platform]; ok && account.Status == service.StatusActive && account.Schedulable {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r *openAIGroupFallbackAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, accounts := range r.accountsByGroup {
		for _, account := range accounts {
			if account.ID == id {
				copy := account
				return &copy, nil
			}
		}
	}
	return nil, nil
}

func filterOpenAIGroupFallbackAccounts(accounts []service.Account, platform string) []service.Account {
	out := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Platform == platform && account.Status == service.StatusActive && account.Schedulable {
			out = append(out, account)
		}
	}
	return out
}

type openAIGroupFallbackGroupRepo struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

type openAIGroupFallbackChannelRepo struct {
	service.ChannelRepository
}

func (r *openAIGroupFallbackChannelRepo) ListAll(context.Context) ([]service.Channel, error) {
	return nil, nil
}

func (r *openAIGroupFallbackGroupRepo) GetByID(_ context.Context, id int64) (*service.Group, error) {
	group := r.groups[id]
	if group == nil {
		return nil, service.ErrGroupNotFound
	}
	return group, nil
}

func (r *openAIGroupFallbackGroupRepo) GetByIDLite(ctx context.Context, id int64) (*service.Group, error) {
	return r.GetByID(ctx, id)
}

type openAIGroupFallbackHTTPUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	statuses map[int64]int
	hangs    map[int64]bool
	calls    []int64
}

func (u *openAIGroupFallbackHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, accountID)
	status := u.statuses[accountID]
	hang := u.hangs[accountID]
	u.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if hang {
		reader, writer := io.Pipe()
		go func() {
			defer func() { _ = writer.Close() }()
			if _, err := io.WriteString(writer, `data: {"type":"response.created","response":{"id":"resp-stalled"}}`+"\n\n"); err != nil {
				return
			}
			<-req.Context().Done()
			_ = writer.CloseWithError(req.Context().Err())
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       reader,
			Request:    req,
		}, nil
	}
	body := `{"id":"chatcmpl-fallback","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"fallback-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	if status >= http.StatusBadRequest {
		body = `{"error":{"message":"temporary upstream failure"}}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (u *openAIGroupFallbackHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *openAIGroupFallbackHTTPUpstream) accountCalls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...)
}

type openAIGroupFallbackUsageRepo struct {
	service.UsageLogRepository
	created chan *service.UsageLog
}

type openAIGroupFallbackUsageBillingRepo struct {
	service.UsageBillingRepository
}

func (r *openAIGroupFallbackUsageBillingRepo) Apply(context.Context, *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	return &service.UsageBillingApplyResult{Applied: false}, nil
}

func (r *openAIGroupFallbackUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	copy := *log
	if log.GroupID != nil {
		groupID := *log.GroupID
		copy.GroupID = &groupID
	}
	r.created <- &copy
	return true, nil
}

type openAIGroupFallbackFixture struct {
	handler     *OpenAIGatewayHandler
	apiKey      *service.APIKey
	accountRepo *openAIGroupFallbackAccountRepo
	upstream    *openAIGroupFallbackHTTPUpstream
	usage       *openAIGroupFallbackUsageRepo
}

func newOpenAIGroupFallbackFixture(t *testing.T, primaryAccounts []service.Account, gatewayRunMode string) *openAIGroupFallbackFixture {
	t.Helper()
	fallbackID := openAIGroupFallbackTargetID
	primaryGroup := &service.Group{
		ID:             openAIGroupFallbackPrimaryID,
		Name:           "0.05",
		Platform:       service.PlatformOpenAI,
		Status:         service.StatusActive,
		RateMultiplier: 0.05,
	}
	fallbackGroup := &service.Group{
		ID:             openAIGroupFallbackTargetID,
		Name:           "0.09",
		Platform:       service.PlatformOpenAI,
		Status:         service.StatusActive,
		RateMultiplier: 0.09,
	}
	fallbackAccount := openAIGroupFallbackAccount(openAIGroupFallbackTargetAcc, openAIGroupFallbackTargetID, nil)
	accountRepo := &openAIGroupFallbackAccountRepo{accountsByGroup: map[int64][]service.Account{
		openAIGroupFallbackPrimaryID: primaryAccounts,
		openAIGroupFallbackTargetID:  {fallbackAccount},
	}}
	groupRepo := &openAIGroupFallbackGroupRepo{groups: map[int64]*service.Group{
		primaryGroup.ID:  primaryGroup,
		fallbackGroup.ID: fallbackGroup,
	}}

	gatewayCfg := &config.Config{}
	gatewayCfg.RunMode = gatewayRunMode
	gatewayCfg.Default.RateMultiplier = 1
	gatewayCfg.Security.URLAllowlist.Enabled = false
	gatewayCfg.Security.URLAllowlist.AllowInsecureHTTP = true
	billingCfg := &config.Config{}
	billingCfg.RunMode = config.RunModeSimple
	billingCfg.Default.RateMultiplier = 1
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, billingCfg, nil)
	usageRepo := &openAIGroupFallbackUsageRepo{created: make(chan *service.UsageLog, 2)}
	upstream := &openAIGroupFallbackHTTPUpstream{statuses: make(map[int64]int), hangs: make(map[int64]bool)}
	channelService := service.NewChannelService(&openAIGroupFallbackChannelRepo{}, groupRepo, nil, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		&openAIGroupFallbackUsageBillingRepo{},
		nil,
		nil,
		nil,
		nil,
		gatewayCfg,
		nil,
		nil,
		service.NewBillingService(gatewayCfg, nil),
		nil,
		billingCache,
		upstream,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		channelService,
		nil,
		nil,
		nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCache,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
		maxAccountSwitches:  1,
		cfg:                 gatewayCfg,
	}
	apiKey := &service.APIKey{
		ID:                                7005,
		UserID:                            8005,
		GroupID:                           &primaryGroup.ID,
		OpenAIAvailabilityFallbackGroupID: &fallbackID,
		Group:                             primaryGroup,
		User:                              &service.User{ID: 8005, Status: service.StatusActive},
		Status:                            service.StatusActive,
	}
	return &openAIGroupFallbackFixture{handler: h, apiKey: apiKey, accountRepo: accountRepo, upstream: upstream, usage: usageRepo}
}

func openAIGroupFallbackAccount(id, groupID int64, modelMapping map[string]string) service.Account {
	credentials := map[string]any{
		"api_key":  fmt.Sprintf("sk-test-%d", id),
		"base_url": "https://fallback.example/v1",
	}
	if len(modelMapping) > 0 {
		rawMapping := make(map[string]any, len(modelMapping))
		for from, to := range modelMapping {
			rawMapping[from] = to
		}
		credentials["model_mapping"] = rawMapping
	}
	return service.Account{
		ID:          id,
		Name:        fmt.Sprintf("account-%d", id),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: credentials,
		Extra:       map[string]any{"openai_responses_supported": false},
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
		GroupIDs:    []int64{groupID},
	}
}

func runOpenAIGroupFallbackRequest(t *testing.T, fixture *openAIGroupFallbackFixture, endpoint, body string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	c.Set(string(middleware2.ContextKeyAPIKey), fixture.apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: fixture.apiKey.UserID, Concurrency: 1})
	if endpoint == EndpointResponses {
		fixture.handler.Responses(c)
	} else {
		fixture.handler.ChatCompletions(c)
	}
	return recorder, c
}

func requireOpenAIGroupFallbackUsage(t *testing.T, fixture *openAIGroupFallbackFixture) {
	t.Helper()
	select {
	case usage := <-fixture.usage.created:
		require.NotNil(t, usage.GroupID)
		require.Equal(t, openAIGroupFallbackTargetID, *usage.GroupID)
		require.Equal(t, openAIGroupFallbackTargetAcc, usage.AccountID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fallback usage record")
	}
}

func TestOpenAIGroupFallback_NoAvailablePrimaryUsesFallbackForGPTHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{name: "responses", endpoint: EndpointResponses, body: `{"model":"gpt-5","input":"hello","stream":false}`},
		{name: "chat_completions", endpoint: "/v1/chat/completions", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newOpenAIGroupFallbackFixture(t, nil, config.RunModeStandard)
			recorder, c := runOpenAIGroupFallbackRequest(t, fixture, tt.endpoint, tt.body)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "fallback-ok")
			require.Equal(t, []int64{openAIGroupFallbackTargetAcc}, fixture.upstream.accountCalls())
			boundAPIKey, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			require.Equal(t, openAIGroupFallbackTargetID, *boundAPIKey.GroupID)
			require.Equal(t, openAIGroupFallbackPrimaryID, *fixture.apiKey.GroupID, "auth snapshot must remain unchanged")
			requireOpenAIGroupFallbackUsage(t, fixture)
		})
	}
}

func TestOpenAIGroupFallback_RetryablePrimaryFailureUsesFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{name: "responses", endpoint: EndpointResponses, body: `{"model":"gpt-5","input":"hello","stream":false}`},
		{name: "chat_completions", endpoint: "/v1/chat/completions", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := openAIGroupFallbackAccount(openAIGroupFallbackPrimaryAcc, openAIGroupFallbackPrimaryID, nil)
			fixture := newOpenAIGroupFallbackFixture(t, []service.Account{primary}, config.RunModeStandard)
			fixture.upstream.statuses[openAIGroupFallbackPrimaryAcc] = http.StatusBadGateway

			recorder, _ := runOpenAIGroupFallbackRequest(t, fixture, tt.endpoint, tt.body)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "fallback-ok")
			require.Equal(t, []int64{openAIGroupFallbackPrimaryAcc, openAIGroupFallbackTargetAcc}, fixture.upstream.accountCalls())
			requireOpenAIGroupFallbackUsage(t, fixture)
		})
	}
}

func TestOpenAIGroupFallback_FirstOutputTimeoutBudgetDoesNotReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	primaryFirst := openAIGroupFallbackAccount(openAIGroupFallbackPrimaryAcc, openAIGroupFallbackPrimaryID, nil)
	primaryFirst.Priority = 1
	primaryFirst.Extra["openai_responses_supported"] = true
	primarySecond := openAIGroupFallbackAccount(openAIGroupFallbackPrimaryAcc+1, openAIGroupFallbackPrimaryID, nil)
	primarySecond.Priority = 2
	primarySecond.Extra["openai_responses_supported"] = true
	fixture := newOpenAIGroupFallbackFixture(t, []service.Account{primaryFirst, primarySecond}, config.RunModeStandard)
	fixture.handler.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1

	fallbackFirst := openAIGroupFallbackAccount(openAIGroupFallbackTargetAcc, openAIGroupFallbackTargetID, nil)
	fallbackFirst.Priority = 1
	fallbackFirst.Extra["openai_responses_supported"] = true
	fallbackSecond := openAIGroupFallbackAccount(openAIGroupFallbackTargetAcc+1, openAIGroupFallbackTargetID, nil)
	fallbackSecond.Priority = 2
	fallbackSecond.Extra["openai_responses_supported"] = true
	fixture.accountRepo.accountsByGroup[openAIGroupFallbackTargetID] = []service.Account{fallbackFirst, fallbackSecond}

	for _, accountID := range []int64{primaryFirst.ID, primarySecond.ID, fallbackFirst.ID, fallbackSecond.ID} {
		fixture.upstream.hangs[accountID] = true
	}

	recorder, _ := runOpenAIGroupFallbackRequest(
		t,
		fixture,
		EndpointResponses,
		`{"model":"gpt-5.5","input":"hello","stream":true}`,
	)

	require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "temporarily unavailable")
	require.Equal(t, []int64{primaryFirst.ID, primarySecond.ID, fallbackFirst.ID}, fixture.upstream.accountCalls())
}

func TestOpenAIGroupFallback_ModelUnsupportedDoesNotSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	primary := openAIGroupFallbackAccount(
		openAIGroupFallbackPrimaryAcc,
		openAIGroupFallbackPrimaryID,
		map[string]string{"other-model": "other-upstream"},
	)
	fixture := newOpenAIGroupFallbackFixture(t, []service.Account{primary}, config.RunModeStandard)

	recorder, c := runOpenAIGroupFallbackRequest(t, fixture, EndpointResponses, `{"model":"gpt-5","input":"hello","stream":false}`)

	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "model_not_found")
	require.Empty(t, fixture.upstream.accountCalls())
	boundAPIKey, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	require.Equal(t, openAIGroupFallbackPrimaryID, *boundAPIKey.GroupID)
}
