package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func optionalInstructionsTestKey(keyEnabled, groupEnabled bool, prompt string) *service.APIKey {
	return &service.APIKey{
		OptionalInstructionsEnabled: keyEnabled,
		Group: &service.Group{
			OptionalInstructionsEnabled: groupEnabled,
			OptionalInstructions:        prompt,
		},
	}
}

func TestInjectOptionalResponsesInstructionsRequiresBothSwitches(t *testing.T) {
	body := []byte(`{"model":"gpt-5","instructions":"client"}`)
	for _, key := range []*service.APIKey{
		optionalInstructionsTestKey(false, false, "admin"),
		optionalInstructionsTestKey(true, false, "admin"),
		optionalInstructionsTestKey(false, true, "admin"),
		optionalInstructionsTestKey(true, true, "  "),
	} {
		got, changed, err := injectOptionalResponsesInstructions(body, key)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, got)
	}
}

func TestInjectOptionalResponsesInstructionsPrependsAndPreservesClient(t *testing.T) {
	got, changed, err := injectOptionalResponsesInstructions(
		[]byte(`{"model":"gpt-5","instructions":"client"}`),
		optionalInstructionsTestKey(true, true, "admin"),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>\n\nclient", gjson.GetBytes(got, "instructions").String())
}

func TestInjectOptionalResponsesInstructionsSupportsNestedWebSocketResponse(t *testing.T) {
	got, changed, err := injectOptionalResponsesInstructions(
		[]byte(`{"type":"response.create","response":{"model":"gpt-5"}}`),
		optionalInstructionsTestKey(true, true, "admin"),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "response.instructions").String())
}

func TestInjectOptionalAnthropicSystemPreservesBlocks(t *testing.T) {
	got, changed, err := injectOptionalAnthropicSystem(
		[]byte(`{"model":"claude","system":[{"type":"text","text":"client"}],"messages":[]}`),
		optionalInstructionsTestKey(true, true, "admin"),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "system.0.text").String())
	require.Equal(t, "client", gjson.GetBytes(got, "system.1.text").String())
}
