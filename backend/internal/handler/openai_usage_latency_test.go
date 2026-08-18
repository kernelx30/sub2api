package handler

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEndToEndUsageResultIncludesRoutingAndPreviousAttempts(t *testing.T) {
	requestStart := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	forwardStart := requestStart.Add(5 * time.Second)
	requestEnd := requestStart.Add(8 * time.Second)
	attemptFirstTokenMs := 250
	result := &service.OpenAIForwardResult{
		Duration:     2 * time.Second,
		FirstTokenMs: &attemptFirstTokenMs,
	}

	usageResult := openAIEndToEndUsageResult(result, requestStart, forwardStart, requestEnd)

	require.NotSame(t, result, usageResult)
	require.Equal(t, 8*time.Second, usageResult.Duration)
	require.NotNil(t, usageResult.FirstTokenMs)
	require.Equal(t, 5250, *usageResult.FirstTokenMs)
	require.Equal(t, 2*time.Second, result.Duration, "scheduler result must remain attempt-local")
	require.Equal(t, 250, *result.FirstTokenMs, "scheduler TTFT must remain attempt-local")
}

func TestOpenAIEndToEndUsageResultNil(t *testing.T) {
	require.Nil(t, openAIEndToEndUsageResult(nil, time.Now(), time.Now(), time.Now()))
}
