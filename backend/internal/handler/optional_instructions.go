package handler

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const optionalInstructionsOpenTag = "<group_optional_instructions>"
const optionalInstructionsCloseTag = "</group_optional_instructions>"

func optionalInstructionsForAPIKey(apiKey *service.APIKey) string {
	if apiKey == nil || !apiKey.OptionalInstructionsEnabled || apiKey.Group == nil ||
		apiKey.Group.Platform != service.PlatformOpenAI || !apiKey.Group.OptionalInstructionsEnabled {
		return ""
	}
	return strings.TrimSpace(apiKey.Group.OptionalInstructions)
}

func formatOptionalInstructions(instructions string) string {
	return optionalInstructionsOpenTag + "\n" + strings.TrimSpace(instructions) + "\n" + optionalInstructionsCloseTag
}

func prependOptionalInstructions(existing, instructions string) string {
	prefix := formatOptionalInstructions(instructions)
	if strings.TrimSpace(existing) == "" {
		return prefix
	}
	if strings.HasPrefix(strings.TrimSpace(existing), prefix) {
		return existing
	}
	return prefix + "\n\n" + existing
}

// injectOptionalChatCompletionsMessages adds a protocol-native instruction
// while leaving every client-provided message, tool rule, and ordering intact.
// It is client-neutral within OpenAI groups so SDKs, desktop clients, and
// compatibility bridges all receive the same group instructions.
func injectOptionalChatCompletionsMessages(body []byte, apiKey *service.APIKey) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey)
	if instructions == "" {
		return body, false, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, err
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return body, false, nil
	}
	// A developer message has higher priority on current GPT models. Prefer it
	// when present, then fall back to the older system role for compatibility.
	for _, targetRole := range []string{"developer", "system"} {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), targetRole) {
				continue
			}
			changed, handled := prependOptionalInstructionsToChatMessage(message, instructions)
			if !handled || !changed {
				if handled {
					return body, false, nil
				}
				continue
			}
			updated, err := json.Marshal(root)
			return updated, err == nil, err
		}
	}
	// Current GPT models give developer messages higher priority than legacy
	// system messages on the Chat Completions compatibility path. When the
	// client did not supply either role, create a developer message so the
	// group instruction has the same effective priority as Responses
	// `instructions`. Existing client messages remain in their original order.
	root["messages"] = append([]any{map[string]any{
		"role":    "developer",
		"content": formatOptionalInstructions(instructions),
	}}, messages...)
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func prependOptionalInstructionsToChatMessage(message map[string]any, instructions string) (changed, handled bool) {
	switch content := message["content"].(type) {
	case string:
		merged := prependOptionalInstructions(content, instructions)
		if merged == content {
			return false, true
		}
		message["content"] = merged
		return true, true
	case []any:
		if contentBlocksContainOptionalInstructions(content) {
			return false, true
		}
		message["content"] = append([]any{map[string]any{
			"type": "text",
			"text": formatOptionalInstructions(instructions),
		}}, content...)
		return true, true
	case nil:
		message["content"] = formatOptionalInstructions(instructions)
		return true, true
	default:
		return false, false
	}
}

func contentBlocksContainOptionalInstructions(blocks []any) bool {
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok {
			if strings.Contains(stringValue(block["text"]), optionalInstructionsOpenTag) {
				return true
			}
		}
	}
	return false
}

func injectOptionalResponsesInstructions(body []byte, apiKey *service.APIKey) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey)
	if instructions == "" {
		return body, false, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, err
	}
	target := root
	if response, ok := root["response"].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(stringValue(root["type"])), "response.create") {
		target = response
	}
	existing, _ := target["instructions"].(string)
	merged := prependOptionalInstructions(existing, instructions)
	if merged == existing {
		return body, false, nil
	}
	target["instructions"] = merged
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func injectOptionalAnthropicSystem(body []byte, apiKey *service.APIKey) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey)
	if instructions == "" {
		return body, false, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, err
	}
	prefixBlock := map[string]any{"type": "text", "text": formatOptionalInstructions(instructions)}
	switch system := root["system"].(type) {
	case string:
		root["system"] = prependOptionalInstructions(system, instructions)
	case []any:
		if contentBlocksContainOptionalInstructions(system) {
			return body, false, nil
		}
		root["system"] = append([]any{prefixBlock}, system...)
	default:
		root["system"] = []any{prefixBlock}
	}
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
