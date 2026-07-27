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
			Platform:                    service.PlatformOpenAI,
			OptionalInstructionsEnabled: groupEnabled,
			OptionalInstructions:        prompt,
		},
	}
}

func TestOptionalInstructionsOnlyApplyToOpenAIGroups(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	key.Group.Platform = service.PlatformAnthropic
	body := []byte(`{"model":"claude","system":"client","messages":[]}`)
	got, changed, err := injectOptionalAnthropicSystem(body, key)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
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

func TestInjectOptionalChatCompletionsPrependsExistingSystem(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"system","content":"client"},{"role":"user","content":"hello"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "system", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>\n\nclient", gjson.GetBytes(got, "messages.0.content").String())
	require.Equal(t, "hello", gjson.GetBytes(got, "messages.1.content").String())
}

func TestInjectOptionalChatCompletionsPrefersDeveloperAndPreservesContentBlocks(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"system","content":"client system"},{"role":"developer","content":[{"type":"text","text":"client developer"}]},{"role":"user","content":"hello"}]}`),
		key,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client system", gjson.GetBytes(got, "messages.0.content").String())
	require.Equal(t, "developer", gjson.GetBytes(got, "messages.1.role").String())
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "messages.1.content.0.text").String())
	require.Equal(t, "client developer", gjson.GetBytes(got, "messages.1.content.1.text").String())

	second, changed, err := injectOptionalChatCompletionsMessages(got, key)
	require.NoError(t, err)
	require.False(t, changed)
	require.JSONEq(t, string(got), string(second))
}

func TestInjectOptionalChatCompletionsAddsSystemWithoutReorderingClientMessages(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "system", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, "user", gjson.GetBytes(got, "messages.1.role").String())
	require.Equal(t, "assistant", gjson.GetBytes(got, "messages.2.role").String())
}

func TestOptionalInstructionsInjectorsAreIdempotent(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	tests := []struct {
		name string
		body []byte
		fn   func([]byte, *service.APIKey) ([]byte, bool, error)
	}{
		{name: "chat", body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), fn: injectOptionalChatCompletionsMessages},
		{name: "anthropic", body: []byte(`{"system":[],"messages":[]}`), fn: injectOptionalAnthropicSystem},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, changed, err := tc.fn(tc.body, key)
			require.NoError(t, err)
			require.True(t, changed)
			second, changed, err := tc.fn(first, key)
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, string(first), string(second))
		})
	}
}
