package handler

import (
	"context"
	"strconv"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type openAIGroupRequestProtocol uint8

const (
	openAIGroupRequestResponses openAIGroupRequestProtocol = iota
	openAIGroupRequestChatCompletions
)

type openAIGroupRequestState struct {
	apiKey         *service.APIKey
	subscription   *service.UserSubscription
	body           []byte
	forwardBody    []byte
	channelMapping service.ChannelMappingResult
}

func (h *OpenAIGatewayHandler) prepareOpenAIGroupRequestState(
	ctx context.Context,
	apiKey *service.APIKey,
	subscription *service.UserSubscription,
	canonicalBody []byte,
	requestedModel string,
	protocol openAIGroupRequestProtocol,
	injectInstructions bool,
) (*openAIGroupRequestState, error) {
	body := canonicalBody
	if apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == service.PlatformOpenAI {
		if cappedBody, changed := service.ApplyOpenAIReasoningEffortPolicy(body, apiKey.Group.MaxReasoningEffort, apiKey.Group.ReasoningEffortMappings); changed {
			body = cappedBody
		}
	}

	mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, requestedModel)
	if injectInstructions {
		var err error
		switch protocol {
		case openAIGroupRequestChatCompletions:
			body, _, err = injectOptionalChatCompletionsMessages(body, apiKey, requestedModel, mapping.MappedModel)
		default:
			body, _, err = injectOptionalResponsesInstructions(body, apiKey, requestedModel, mapping.MappedModel)
		}
		if err != nil {
			return nil, err
		}
	}

	return &openAIGroupRequestState{
		apiKey:         apiKey,
		subscription:   subscription,
		body:           body,
		forwardBody:    openAIModelMappedBody(body, mapping.Mapped, mapping.MappedModel, h.gatewayService.ReplaceModelInBody),
		channelMapping: mapping,
	}, nil
}

func (h *OpenAIGatewayHandler) resolveOpenAIAvailabilityFallbackState(
	ctx context.Context,
	currentAPIKey *service.APIKey,
	canonicalBody []byte,
	requestedModel string,
	protocol openAIGroupRequestProtocol,
	injectInstructions bool,
) (*openAIGroupRequestState, error) {
	resolved, err := h.gatewayService.ResolveOpenAIAvailabilityFallback(ctx, currentAPIKey)
	if err != nil || resolved == nil {
		return nil, err
	}
	return h.prepareOpenAIGroupRequestState(
		ctx,
		resolved.APIKey,
		resolved.Subscription,
		canonicalBody,
		requestedModel,
		protocol,
		injectInstructions,
	)
}

func (h *OpenAIGatewayHandler) checkOpenAIAvailabilityFallbackBilling(c *gin.Context, state *openAIGroupRequestState) error {
	return h.billingCacheService.CheckBillingEligibilityForGroupFailover(
		c.Request.Context(),
		state.apiKey.User,
		state.apiKey,
		state.apiKey.Group,
		state.subscription,
		service.QuotaPlatform(c.Request.Context(), state.apiKey),
	)
}

func bindOpenAIGroupRequestState(c *gin.Context, state *openAIGroupRequestState) {
	if c == nil || state == nil || state.apiKey == nil {
		return
	}
	c.Set(string(middleware2.ContextKeyAPIKey), state.apiKey)
	c.Set(string(middleware2.ContextKeySubscription), state.subscription)
}

func (h *OpenAIGatewayHandler) writeOpenAIBillingError(c *gin.Context, err error, streamStarted bool) {
	status, code, message, retryAfter := billingErrorDetails(err)
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	h.handleStreamingAwareError(c, status, code, message, streamStarted)
}
