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
	got, changed, err := injectOptionalAnthropicSystem(body, key, "claude")
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
		got, changed, err := injectOptionalResponsesInstructions(body, key, "gpt-5")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, got)
	}
}

func TestInjectOptionalResponsesInstructionsAppendsAfterClient(t *testing.T) {
	got, changed, err := injectOptionalResponsesInstructions(
		[]byte(`{"model":"gpt-5","instructions":"client"}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client\n\n<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "instructions").String())
}

func TestInjectOptionalResponsesInstructionsSupportsFlatWebSocketResponse(t *testing.T) {
	got, changed, err := injectOptionalResponsesInstructions(
		[]byte(`{"type":"response.create","model":"gpt-5"}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "instructions").String())
	require.Equal(t, "gpt-5", gjson.GetBytes(got, "model").String())
}

func TestInjectOptionalAnthropicSystemPreservesBlocks(t *testing.T) {
	got, changed, err := injectOptionalAnthropicSystem(
		[]byte(`{"model":"claude","system":[{"type":"text","text":"client"}],"messages":[]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"claude",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client", gjson.GetBytes(got, "system.0.text").String())
	require.Equal(t, "<group_optional_instructions>\nadmin\n</group_optional_instructions>", gjson.GetBytes(got, "system.1.text").String())
}

func TestInjectOptionalChatCompletionsAppendsToExistingSystem(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"system","content":"client"},{"role":"user","content":"hello"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "system", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, "client\n\n"+formatOptionalInstructions("admin"), gjson.GetBytes(got, "messages.0.content").String())
	require.Equal(t, "hello", gjson.GetBytes(got, "messages.1.content").String())
}

func TestInjectOptionalChatCompletionsAppendsToSystemContentBlocks(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"system","content":[{"type":"text","text":"client system"}]},{"role":"developer","content":"client developer"},{"role":"user","content":"hello"}]}`),
		key,
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client system", gjson.GetBytes(got, "messages.0.content.0.text").String())
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(got, "messages.0.content.1.text").String())
	require.Equal(t, "developer", gjson.GetBytes(got, "messages.1.role").String())
	require.Equal(t, "client developer", gjson.GetBytes(got, "messages.1.content").String())

	second, changed, err := injectOptionalChatCompletionsMessages(got, key, "gpt-5.6")
	require.NoError(t, err)
	require.False(t, changed)
	require.JSONEq(t, string(got), string(second))
}

func TestInjectOptionalChatCompletionsAddsSystemWithoutReorderingConversation(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "system", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, "user", gjson.GetBytes(got, "messages.1.role").String())
	require.Equal(t, "assistant", gjson.GetBytes(got, "messages.2.role").String())
}

func TestInjectOptionalChatCompletionsUsesDeveloperForGPT55(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt-5.5","messages":[{"role":"system","content":"client system"},{"role":"user","content":"hello"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5.5",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "developer", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(got, "messages.0.content").String())
	require.Equal(t, "system", gjson.GetBytes(got, "messages.1.role").String())
	require.Equal(t, "client system", gjson.GetBytes(got, "messages.1.content").String())
}

func TestInjectOptionalChatCompletionsHandlesResponsesShapedPayload(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5.6-sol",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(got, "instructions").String())
	require.Equal(t, "user", gjson.GetBytes(got, "input.0.role").String())
}

func TestOptionalInstructionsInjectorsAreIdempotent(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	tests := []struct {
		name string
		body []byte
		fn   func([]byte, *service.APIKey, ...string) ([]byte, bool, error)
	}{
		{name: "chat", body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`), fn: injectOptionalChatCompletionsMessages},
		{name: "responses", body: []byte(`{"input":"hello"}`), fn: injectOptionalResponsesInstructions},
		{name: "anthropic", body: []byte(`{"system":[],"messages":[]}`), fn: injectOptionalAnthropicSystem},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, changed, err := tc.fn(tc.body, key, "gpt-5.6-sol")
			require.NoError(t, err)
			require.True(t, changed)
			second, changed, err := tc.fn(first, key, "gpt-5.6-sol")
			require.NoError(t, err)
			require.False(t, changed)
			require.JSONEq(t, string(first), string(second))
		})
	}
}

func TestOptionalInstructionsOpenTagAloneDoesNotSuppressInjection(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")

	chat, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"messages":[{"role":"developer","content":[{"type":"text","text":"<group_optional_instructions>"}]}]}`),
		key,
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "system", gjson.GetBytes(chat, "messages.0.role").String())
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(chat, "messages.0.content").String())
	require.Equal(t, "developer", gjson.GetBytes(chat, "messages.1.role").String())
	require.Equal(t, optionalInstructionsOpenTag, gjson.GetBytes(chat, "messages.1.content.0.text").String())

	anthropic, changed, err := injectOptionalAnthropicSystem(
		[]byte(`{"system":[{"type":"text","text":"<group_optional_instructions>"}],"messages":[]}`),
		key,
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, optionalInstructionsOpenTag, gjson.GetBytes(anthropic, "system.0.text").String())
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(anthropic, "system.1.text").String())
}

func TestInjectOptionalChatCompletionsAppendsToLastSystemMessage(t *testing.T) {
	got, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"messages":[{"role":"system","content":"first"},{"role":"system","content":"last"},{"role":"user","content":"hello"}]}`),
		optionalInstructionsTestKey(true, true, "admin"),
		"gpt-5.6",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "first", gjson.GetBytes(got, "messages.0.content").String())
	require.Equal(t, "last\n\n"+formatOptionalInstructions("admin"), gjson.GetBytes(got, "messages.1.content").String())
}

func TestOptionalInstructionsInjectorsPreserveLargeJSONIntegers(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "admin")
	tests := []struct {
		name string
		body []byte
		fn   func([]byte, *service.APIKey, ...string) ([]byte, bool, error)
	}{
		{name: "chat", body: []byte(`{"metadata":{"id":9007199254740993},"messages":[{"role":"user","content":"hello"}]}`), fn: injectOptionalChatCompletionsMessages},
		{name: "responses", body: []byte(`{"metadata":{"id":9007199254740993},"input":"hello"}`), fn: injectOptionalResponsesInstructions},
		{name: "anthropic", body: []byte(`{"metadata":{"id":9007199254740993},"system":"client","messages":[]}`), fn: injectOptionalAnthropicSystem},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := tc.fn(tc.body, key, "gpt-5.6-sol")
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, "9007199254740993", gjson.GetBytes(got, "metadata.id").Raw)
		})
	}
}

func TestOptionalInstructionsModelScopeMatchesGPT55AndGPT56FamiliesAndStripsDirective(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "[MODEL_SCOPE: gpt-5.5*, gpt-5.6*]\n[MODE: UNRESTRICTED]")
	tests := []struct {
		model   string
		matched bool
	}{
		{model: "gpt-5.5", matched: true},
		{model: "gpt-5.5-2026-04-23", matched: true},
		{model: "gpt-5.5-pro", matched: true},
		{model: "openai/gpt-5.5-pro-2026-04-23", matched: true},
		{model: "gpt-5.6", matched: true},
		{model: "gpt-5.6-sol", matched: true},
		{model: "models/gpt-5.6-terra", matched: true},
		{model: "openai/gpt-5.6-luna", matched: true},
		{model: "gpt-5.4", matched: false},
		{model: "claude-opus-4-1", matched: false},
		{model: "", matched: false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			body := []byte(`{"input":"hello"}`)
			got, changed, err := injectOptionalResponsesInstructions(body, key, tc.model)
			require.NoError(t, err)
			require.Equal(t, tc.matched, changed)
			if !tc.matched {
				require.Equal(t, body, got)
				return
			}
			injected := gjson.GetBytes(got, "instructions").String()
			require.Contains(t, injected, "[MODE: UNRESTRICTED]")
			require.NotContains(t, injected, "MODEL_SCOPE")
		})
	}
}

func TestOptionalInstructionsModelScopeMatchesAnyEffectiveModelCandidate(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "[MODEL_SCOPE: gpt-5.5*, gpt-5.6*]\nadmin")
	body := []byte(`{"model":"claude-opus-4-6","system":"client","messages":[]}`)

	got, changed, err := injectOptionalAnthropicSystem(
		body,
		key,
		"claude-opus-4-6",
		"gpt-5.6-sol",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client\n\n"+formatOptionalInstructions("admin"), gjson.GetBytes(got, "system").String())

	got, changed, err = injectOptionalAnthropicSystem(
		body,
		key,
		"claude-opus-4-6",
		"gpt-5.5-pro",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client\n\n"+formatOptionalInstructions("admin"), gjson.GetBytes(got, "system").String())

	got, changed, err = injectOptionalAnthropicSystem(
		body,
		key,
		"claude-opus-4-6",
		"gpt-5.4",
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOptionalInstructionsAccountMappingParticipatesInModelScope(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "[MODEL_SCOPE: gpt-5.6*]\nadmin")
	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"channel-alias": "gpt-5.6-sol",
			},
		},
	}
	accountModel := optionalInstructionsAccountMappedModel(account, "public-gpt", "channel-alias")
	require.Equal(t, "gpt-5.6-sol", accountModel)

	body := []byte(`{"model":"public-gpt","input":"hello"}`)
	got, changed, err := injectOptionalResponsesInstructions(body, key, "public-gpt", accountModel)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(got, "instructions").String())

	chat, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"model":"public-gpt","messages":[{"role":"user","content":"hello"}]}`),
		key,
		"public-gpt",
		accountModel,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(chat, "messages.0.content").String())

	messages, changed, err := injectOptionalAnthropicSystem(
		[]byte(`{"model":"public-gpt","messages":[{"role":"user","content":"hello"}]}`),
		key,
		"public-gpt",
		accountModel,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(messages, "system.0.text").String())
}

func TestOptionalInstructionsMalformedModelScopeFailsClosed(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	got, changed, err := injectOptionalResponsesInstructions(
		body,
		optionalInstructionsTestKey(true, true, "[MODEL_SCOPE gpt-5.6*]\nadmin"),
		"gpt-5.6-sol",
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOptionalInstructionsModelScopeHandlesWhitespaceBeforeBOM(t *testing.T) {
	body := []byte(`{"input":"hello"}`)
	key := optionalInstructionsTestKey(true, true, " \n\ufeff[MODEL_SCOPE: gpt-5.6*]\nadmin")

	got, changed, err := injectOptionalResponsesInstructions(body, key, "gpt-5.4")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)

	got, changed, err = injectOptionalResponsesInstructions(body, key, "gpt-5.6-sol")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(got, "instructions").String())
}

func TestOptionalInstructionsModelScopeAppliesAcrossRequestProtocols(t *testing.T) {
	key := optionalInstructionsTestKey(true, true, "[model_scope: gpt-5.5*, gpt-5.6*]\nadmin")

	chat, changed, err := injectOptionalChatCompletionsMessages(
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		key,
		"gpt-5.6-sol",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(chat, "messages.0.content").String())

	responses, changed, err := injectOptionalResponsesInstructions(
		[]byte(`{"type":"response.create","model":"gpt-5.6"}`),
		key,
		"gpt-5.5",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, formatOptionalInstructions("admin"), gjson.GetBytes(responses, "instructions").String())

	anthropic, changed, err := injectOptionalAnthropicSystem(
		[]byte(`{"system":"client","messages":[]}`),
		key,
		"openai/gpt-5.5-pro-2026-04-23",
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "client\n\n"+formatOptionalInstructions("admin"), gjson.GetBytes(anthropic, "system").String())

	anthropic, changed, err = injectOptionalAnthropicSystem(
		[]byte(`{"system":"client","messages":[]}`),
		key,
		"gpt-5.4",
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, "client", gjson.GetBytes(anthropic, "system").String())
}
