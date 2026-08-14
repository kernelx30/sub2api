package admin

import (
	"sort"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type upstreamBillingProbeEnabledRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

type upstreamBillingProbeBatchRequest struct {
	AccountIDs []int64 `json:"account_ids" binding:"required"`
}

type poolAutoPriorityRankingItem struct {
	Rank                *int       `json:"rank"`
	CohortRank          *int       `json:"cohort_rank"`
	AccountID           int64      `json:"account_id"`
	AccountName         string     `json:"account_name"`
	GroupID             int64      `json:"group_id"`
	GroupName           string     `json:"group_name,omitempty"`
	ManualPriority      int        `json:"manual_priority"`
	Schedulable         bool       `json:"schedulable"`
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

type poolAutoPriorityRankingResponse struct {
	Enabled         bool                          `json:"enabled"`
	IntervalMinutes int                           `json:"interval_minutes"`
	GroupID         int64                         `json:"group_id"`
	GeneratedAt     time.Time                     `json:"generated_at"`
	Total           int                           `json:"total"`
	Items           []poolAutoPriorityRankingItem `json:"items"`
}

func (h *AccountHandler) GetUpstreamBillingProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	settings, err := h.upstreamBillingProbe.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) UpdateUpstreamBillingProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req service.UpstreamBillingProbeSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.UpdateSettings(c.Request.Context(), &req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings, err := h.upstreamBillingProbe.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) GetPoolAutoPrioritySettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	settings, err := h.upstreamBillingProbe.GetPoolAutoPrioritySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// GetPoolAutoPriorityRanking returns persisted probe evidence only. It never
// triggers upstream requests and uses the same probe cohorts as runtime OpenAI
// scheduling.
func (h *AccountHandler) GetPoolAutoPriorityRanking(c *gin.Context) {
	if h.upstreamBillingProbe == nil || h.adminService == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}

	groupID, err := strconv.ParseInt(c.Query("group_id"), 10, 64)
	if err != nil || groupID <= 0 {
		response.BadRequest(c, "group_id must be a positive integer")
		return
	}
	limit := 10
	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsedLimit, parseErr := strconv.Atoi(rawLimit)
		if parseErr != nil || parsedLimit < 1 || parsedLimit > 50 {
			response.BadRequest(c, "limit must be between 1 and 50")
			return
		}
		limit = parsedLimit
	}

	ctx := c.Request.Context()
	settings, err := h.upstreamBillingProbe.GetPoolAutoPrioritySettings(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(
		ctx,
		service.PlatformOpenAI,
		"apikey",
		"",
		"",
		groupID,
		"",
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	now := time.Now().UTC()
	participating := make([]*service.Account, 0, len(accounts))
	schedulable := make([]*service.Account, 0, len(accounts))
	accountByID := make(map[int64]*service.Account, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if account.Platform != service.PlatformOpenAI || account.Type != "apikey" || !service.IsPoolAutoPriorityEnabled(account) {
			continue
		}
		participating = append(participating, account)
		accountByID[account.ID] = account
		if account.IsSchedulable() {
			schedulable = append(schedulable, account)
		}
	}

	items := make([]poolAutoPriorityRankingItem, 0, len(participating))
	rankedIDs := make(map[int64]struct{}, len(schedulable))
	for _, snapshot := range service.BuildPoolAutoPriorityRanking(schedulable, now) {
		account := accountByID[snapshot.AccountID]
		items = append(items, poolAutoPriorityRankingItemFromSnapshot(account, groupID, snapshot, true))
		rankedIDs[snapshot.AccountID] = struct{}{}
	}
	for _, account := range participating {
		if _, ok := rankedIDs[account.ID]; ok {
			continue
		}
		snapshots := service.BuildPoolAutoPriorityRanking([]*service.Account{account}, now)
		if len(snapshots) == 0 {
			continue
		}
		items = append(items, poolAutoPriorityRankingItemFromSnapshot(account, groupID, snapshots[0], false))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Rank != nil && items[j].Rank == nil {
			return true
		}
		if items[i].Rank == nil && items[j].Rank != nil {
			return false
		}
		if items[i].Rank != nil && items[j].Rank != nil && *items[i].Rank != *items[j].Rank {
			return *items[i].Rank < *items[j].Rank
		}
		return items[i].AccountID < items[j].AccountID
	})
	total := len(items)
	if len(items) > limit {
		items = items[:limit]
	}
	response.Success(c, poolAutoPriorityRankingResponse{
		Enabled:         settings.Enabled,
		IntervalMinutes: settings.IntervalMinutes,
		GroupID:         groupID,
		GeneratedAt:     now,
		Total:           total,
		Items:           items,
	})
}

func poolAutoPriorityRankingItemFromSnapshot(
	account *service.Account,
	groupID int64,
	snapshot service.PoolAutoPriorityRankingSnapshot,
	ranked bool,
) poolAutoPriorityRankingItem {
	item := poolAutoPriorityRankingItem{
		AccountID:           snapshot.AccountID,
		GroupID:             groupID,
		ManualPriority:      snapshot.ManualPriority,
		Schedulable:         ranked,
		ProbeStatus:         snapshot.ProbeStatus,
		ProbeModel:          snapshot.ProbeModel,
		SampleCount:         snapshot.SampleCount,
		SuccessRate:         snapshot.SuccessRate,
		P50LatencyMS:        snapshot.P50LatencyMS,
		P95LatencyMS:        snapshot.P95LatencyMS,
		LatestLatencyMS:     snapshot.LatestLatencyMS,
		ConsecutiveFailures: snapshot.ConsecutiveFailures,
		LastProbeAt:         snapshot.LastProbeAt,
		NextProbeAt:         snapshot.NextProbeAt,
		AvailableBalance:    snapshot.AvailableBalance,
		BalanceUnlimited:    snapshot.BalanceUnlimited,
		BalanceSource:       snapshot.BalanceSource,
	}
	if ranked {
		rank := snapshot.Rank
		cohortRank := snapshot.CohortRank
		item.Rank = &rank
		item.CohortRank = &cohortRank
	}
	if account != nil {
		item.AccountName = account.Name
		for _, group := range account.Groups {
			if group != nil && group.ID == groupID {
				item.GroupName = group.Name
				break
			}
		}
	}
	return item
}

func (h *AccountHandler) UpdatePoolAutoPrioritySettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req service.PoolAutoPrioritySettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.UpdatePoolAutoPrioritySettings(c.Request.Context(), &req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	settings, err := h.upstreamBillingProbe.GetPoolAutoPrioritySettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) SetUpstreamBillingProbeEnabled(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req upstreamBillingProbeEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.SetAccountEnabled(c.Request.Context(), accountID, *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_id": accountID, "enabled": *req.Enabled})
}

func (h *AccountHandler) SetPoolAutoPriorityEnabled(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req upstreamBillingProbeEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.SetPoolAutoPriorityEnabled(c.Request.Context(), accountID, *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_id": accountID, "enabled": *req.Enabled})
}

func (h *AccountHandler) ProbeUpstreamBilling(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	snapshot, err := h.upstreamBillingProbe.ProbeAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, service.UpstreamBillingProbeResult{AccountID: accountID, Snapshot: snapshot})
}

func (h *AccountHandler) ProbeUpstreamBillingBatch(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req upstreamBillingProbeBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 || len(req.AccountIDs) > service.UpstreamBillingProbeMaxBatchSize {
		response.BadRequest(c, "account_ids must contain between 1 and 20 items")
		return
	}
	seen := make(map[int64]struct{}, len(req.AccountIDs))
	accountIDs := make([]int64, 0, len(req.AccountIDs))
	for _, accountID := range req.AccountIDs {
		if accountID <= 0 {
			response.BadRequest(c, "account_ids must contain positive IDs")
			return
		}
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		accountIDs = append(accountIDs, accountID)
	}
	response.Success(c, gin.H{"results": h.upstreamBillingProbe.ProbeAccounts(c.Request.Context(), accountIDs)})
}
