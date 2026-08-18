package service

import (
	"context"
	"time"
)

// OpenAIAccountRuntimeStatSnapshot is a Redis-backed view of recent real
// request outcomes. SampleCount counts positive TTFT observations.
type OpenAIAccountRuntimeStatSnapshot struct {
	ErrorRateEWMA float64
	TTFTEWMA      float64
	SampleCount   int64
	TTFTUpdatedAt time.Time
	LastFailureAt time.Time
	LastSuccessAt time.Time
}

// OpenAIAccountRuntimeStatsStore is an optional capability implemented by the
// production gateway cache. Keeping it separate avoids widening GatewayCache.
type OpenAIAccountRuntimeStatsStore interface {
	Report(
		ctx context.Context,
		accountID int64,
		model string,
		success bool,
		firstTokenMs *int,
		observedAt time.Time,
		ttl time.Duration,
	) error
	Load(
		ctx context.Context,
		accountID int64,
		model string,
	) (account OpenAIAccountRuntimeStatSnapshot, modelSnapshot OpenAIAccountRuntimeStatSnapshot, err error)
}

// NormalizeOpenAIAccountRuntimeModel keeps persisted model-scoped statistics
// aligned with the scheduler's existing account+model key normalization.
func NormalizeOpenAIAccountRuntimeModel(model string) string {
	return normalizeOpenAIAccountModelTransientModel(model)
}
