package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIChannelMappedRequestModel(t *testing.T) {
	t.Run("channel mapping wins", func(t *testing.T) {
		got := openAIChannelMappedRequestModel(" public-model ", service.ChannelMappingResult{
			Mapped:      true,
			MappedModel: " upstream-model ",
		})
		require.Equal(t, "upstream-model", got)
	})

	t.Run("requested model is the fallback", func(t *testing.T) {
		got := openAIChannelMappedRequestModel(" public-model ", service.ChannelMappingResult{
			MappedModel: "ignored-model",
		})
		require.Equal(t, "public-model", got)
	})
}

func TestOpenAIMessagesSchedulingModelPrecedence(t *testing.T) {
	t.Run("channel mapping is authoritative", func(t *testing.T) {
		got := openAIMessagesSchedulingModel("routing-model", "preferred-model", service.ChannelMappingResult{
			Mapped:      true,
			MappedModel: " channel-model ",
		})
		require.Equal(t, "channel-model", got)
	})

	t.Run("preferred mapping is the legacy fallback", func(t *testing.T) {
		got := openAIMessagesSchedulingModel("routing-model", " preferred-model ", service.ChannelMappingResult{})
		require.Equal(t, "preferred-model", got)
	})

	t.Run("routing model is the final fallback", func(t *testing.T) {
		got := openAIMessagesSchedulingModel(" routing-model ", "", service.ChannelMappingResult{})
		require.Equal(t, "routing-model", got)
	})
}
