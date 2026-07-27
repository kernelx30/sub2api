package handler

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const optionalInstructionsOpenTag = "<group_optional_instructions>"
const optionalInstructionsCloseTag = "</group_optional_instructions>"

func optionalInstructionsForAPIKey(apiKey *service.APIKey) string {
	if apiKey == nil || !apiKey.OptionalInstructionsEnabled || apiKey.Group == nil || !apiKey.Group.OptionalInstructionsEnabled {
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
