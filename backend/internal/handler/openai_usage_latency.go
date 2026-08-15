package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// openAIEndToEndUsageResult keeps scheduler latency attempt-local while making
// the persisted usage row reflect the latency observed by the client.
func openAIEndToEndUsageResult(
	result *service.OpenAIForwardResult,
	requestStart time.Time,
	forwardStart time.Time,
	requestEnd time.Time,
) *service.OpenAIForwardResult {
	if result == nil {
		return nil
	}
	usageResult := *result
	if requestStart.IsZero() {
		return &usageResult
	}
	if requestEnd.Before(requestStart) {
		requestEnd = requestStart
	}
	usageResult.Duration = requestEnd.Sub(requestStart)
	if result.FirstTokenMs != nil {
		beforeFinalAttempt := forwardStart.Sub(requestStart)
		if beforeFinalAttempt < 0 {
			beforeFinalAttempt = 0
		}
		firstTokenMs := int(beforeFinalAttempt.Milliseconds()) + *result.FirstTokenMs
		usageResult.FirstTokenMs = &firstTokenMs
	}
	return &usageResult
}
