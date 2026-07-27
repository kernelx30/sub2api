package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const optionalInstructionsOpenTag = "<group_optional_instructions>"
const optionalInstructionsCloseTag = "</group_optional_instructions>"
const optionalInstructionsModelScopePrefix = "[MODEL_SCOPE:"

func optionalInstructionsForAPIKey(apiKey *service.APIKey, candidateModels ...string) string {
	if apiKey == nil || !apiKey.OptionalInstructionsEnabled || !service.GroupOffersOptionalInstructions(apiKey.Group) {
		return ""
	}

	instructions, modelPatterns, scoped := parseOptionalInstructionsModelScope(apiKey.Group.OptionalInstructions)
	if instructions == "" {
		return ""
	}
	if scoped {
		for _, model := range candidateModels {
			if optionalInstructionsModelMatches(model, modelPatterns) {
				return instructions
			}
		}
		return ""
	}
	return instructions
}

// parseOptionalInstructionsModelScope recognizes an optional first-line directive:
// [MODEL_SCOPE: gpt-5.5*, gpt-5.6*]. The directive controls injection and is not sent upstream.
func parseOptionalInstructionsModelScope(raw string) (instructions string, modelPatterns []string, scoped bool) {
	instructions = strings.TrimSpace(raw)
	instructions = strings.TrimSpace(strings.TrimPrefix(instructions, "\ufeff"))
	if instructions == "" {
		return "", nil, false
	}

	firstLine := instructions
	remainder := ""
	if newline := strings.IndexByte(instructions, '\n'); newline >= 0 {
		firstLine = instructions[:newline]
		remainder = instructions[newline+1:]
	}
	firstLine = strings.TrimSpace(strings.TrimSuffix(firstLine, "\r"))
	upperFirstLine := strings.ToUpper(firstLine)
	if !strings.HasPrefix(upperFirstLine, "[MODEL_SCOPE") {
		return instructions, nil, false
	}
	// A malformed scope-like directive must not fall back to all-model injection.
	if !strings.HasPrefix(upperFirstLine, optionalInstructionsModelScopePrefix) || !strings.HasSuffix(firstLine, "]") {
		return strings.TrimSpace(remainder), nil, true
	}

	rawPatterns := firstLine[len(optionalInstructionsModelScopePrefix) : len(firstLine)-1]
	modelPatterns = strings.FieldsFunc(rawPatterns, func(r rune) bool {
		return r == ',' || r == ';' || r == '；' || unicode.IsSpace(r)
	})
	for i := range modelPatterns {
		modelPatterns[i] = strings.TrimSpace(modelPatterns[i])
	}
	return strings.TrimSpace(remainder), modelPatterns, true
}

func optionalInstructionsModelMatches(requestedModel string, modelPatterns []string) bool {
	model := strings.ToLower(strings.TrimSpace(requestedModel))
	if model == "" || len(modelPatterns) == 0 {
		return false
	}

	candidates := []string{model}
	if unprefixed := strings.TrimPrefix(model, "models/"); unprefixed != model {
		candidates = append(candidates, unprefixed)
	}
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 && slash+1 < len(model) {
		candidates = append(candidates, model[slash+1:])
	}

	for _, rawPattern := range modelPatterns {
		pattern := strings.ToLower(strings.TrimSpace(rawPattern))
		if pattern == "" {
			continue
		}
		for _, candidate := range candidates {
			if pattern == "*" || candidate == pattern ||
				(strings.HasSuffix(pattern, "*") && strings.HasPrefix(candidate, strings.TrimSuffix(pattern, "*"))) {
				return true
			}
		}
	}
	return false
}

// optionalInstructionsAccountMappedModel mirrors the final account-level
// mapping input: channel mapping is applied first, then account mapping.
func optionalInstructionsAccountMappedModel(account *service.Account, requestedModel, channelMappedModel string) string {
	if account == nil {
		return ""
	}
	effectiveModel := strings.TrimSpace(channelMappedModel)
	if effectiveModel == "" {
		effectiveModel = strings.TrimSpace(requestedModel)
	}
	return strings.TrimSpace(account.GetMappedModel(effectiveModel))
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
func injectOptionalChatCompletionsMessages(body []byte, apiKey *service.APIKey, candidateModels ...string) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey, candidateModels...)
	if instructions == "" {
		return body, false, nil
	}
	root, err := decodeOptionalInstructionsObject(body)
	if err != nil {
		return nil, false, err
	}
	rawMessages, hasMessages := root["messages"]
	if !hasMessages {
		// Cursor and a few compatible clients send a Responses-shaped payload to
		// /v1/chat/completions. The service forwards that shape as Responses, so
		// inject into its native instructions field instead of silently skipping.
		if _, hasInput := root["input"]; hasInput {
			return injectOptionalResponsesInstructions(body, apiKey, candidateModels...)
		}
		return body, false, nil
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return body, false, nil
	}
	// Current GPT models prioritize developer messages over the legacy system
	// role. Keep client system messages untouched and inject only into an
	// existing developer message or a new leading developer message.
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "developer") {
			continue
		}
		changed, handled := prependOptionalInstructionsToChatMessage(message, instructions)
		if !handled {
			continue
		}
		if !changed {
			return body, false, nil
		}
		updated, err := json.Marshal(root)
		return updated, err == nil, err
	}
	// The new message is prepended without changing the order or contents of
	// any client-provided messages.
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
		if contentBlocksStartWithOptionalInstructions(content, instructions) {
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

func contentBlocksStartWithOptionalInstructions(blocks []any, instructions string) bool {
	if len(blocks) == 0 {
		return false
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "text") {
		return false
	}
	return strings.TrimSpace(stringValue(block["text"])) == formatOptionalInstructions(instructions)
}

func injectOptionalResponsesInstructions(body []byte, apiKey *service.APIKey, candidateModels ...string) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey, candidateModels...)
	if instructions == "" {
		return body, false, nil
	}
	root, err := decodeOptionalInstructionsObject(body)
	if err != nil {
		return nil, false, err
	}
	var existing string
	if raw, exists := root["instructions"]; exists && raw != nil {
		var ok bool
		existing, ok = raw.(string)
		if !ok {
			return nil, false, errors.New("instructions must be a string or null")
		}
	}
	merged := prependOptionalInstructions(existing, instructions)
	if merged == existing {
		return body, false, nil
	}
	root["instructions"] = merged
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func injectOptionalAnthropicSystem(body []byte, apiKey *service.APIKey, candidateModels ...string) ([]byte, bool, error) {
	instructions := optionalInstructionsForAPIKey(apiKey, candidateModels...)
	if instructions == "" {
		return body, false, nil
	}
	root, err := decodeOptionalInstructionsObject(body)
	if err != nil {
		return nil, false, err
	}
	prefixBlock := map[string]any{"type": "text", "text": formatOptionalInstructions(instructions)}
	switch system := root["system"].(type) {
	case string:
		root["system"] = prependOptionalInstructions(system, instructions)
	case []any:
		if contentBlocksStartWithOptionalInstructions(system, instructions) {
			return body, false, nil
		}
		root["system"] = append([]any{prefixBlock}, system...)
	case nil:
		root["system"] = []any{prefixBlock}
	default:
		return nil, false, errors.New("system must be a string, array, or null")
	}
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func decodeOptionalInstructionsObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("request body must be a JSON object")
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("request body must contain a single JSON object")
		}
		return nil, err
	}
	return root, nil
}
