package service

import (
	"log/slog"
	"strings"
)

const openAISlowTTFTLogThresholdMS = int64(10000)

func logOpenAISlowTTFTStages(
	account *Account,
	model string,
	requestID string,
	firstTokenMS int,
	prepareMS int64,
	responseHeadersMS int64,
	bodyBytes int,
	passthrough bool,
) {
	if account == nil || int64(firstTokenMS) < openAISlowTTFTLogThresholdMS {
		return
	}
	afterHeadersMS := int64(firstTokenMS) - prepareMS - responseHeadersMS
	if afterHeadersMS < 0 {
		afterHeadersMS = 0
	}
	slog.Warn("openai_slow_ttft_stages",
		"account_id", account.ID,
		"account_name", strings.TrimSpace(account.Name),
		"model", strings.TrimSpace(model),
		"request_id", strings.TrimSpace(requestID),
		"first_token_ms", firstTokenMS,
		"prepare_ms", prepareMS,
		"response_headers_ms", responseHeadersMS,
		"after_headers_ms", afterHeadersMS,
		"body_bytes", bodyBytes,
		"proxy", account.ProxyID != nil,
		"passthrough", passthrough,
	)
}
