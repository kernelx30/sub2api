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

func appendOptionalInstructions(existing, instructions string) string {
	suffix := formatOptionalInstructions(instructions)
	if strings.TrimSpace(existing) == "" {
		return suffix
	}
	if strings.HasSuffix(strings.TrimSpace(existing), suffix) {
		return existing
	}
	return existing + "\n\n" + suffix
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
	instructionRole := "system"
	// The tested GPT-5.5/GPT-5.6 routes follow system messages more reliably than
	// developer messages. Append to the last system message so the group layer
	// remains the final same-role behavior instruction.
	for i := len(messages) - 1; i >= 0; i-- {
		raw := messages[i]
		message, ok := raw.(map[string]any)
		if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), instructionRole) {
			continue
		}
		changed, handled := appendOptionalInstructionsToChatMessage(message, instructions)
		if !handled {
			continue
		}
		if !changed {
			return body, false, nil
		}
		updated, err := json.Marshal(root)
		return updated, err == nil, err
	}
	// No matching client instruction message exists, so add a leading one without
	// changing the order or contents of any client-provided messages.
	root["messages"] = append([]any{map[string]any{
		"role":    instructionRole,
		"content": formatOptionalInstructions(instructions),
	}}, messages...)
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func appendOptionalInstructionsToChatMessage(message map[string]any, instructions string) (changed, handled bool) {
	switch content := message["content"].(type) {
	case string:
		merged := appendOptionalInstructions(content, instructions)
		if merged == content {
			return false, true
		}
		message["content"] = merged
		return true, true
	case []any:
		if contentBlocksEndWithOptionalInstructions(content, instructions) {
			return false, true
		}
		message["content"] = append(content, map[string]any{
			"type": "text",
			"text": formatOptionalInstructions(instructions),
		})
		return true, true
	case nil:
		message["content"] = formatOptionalInstructions(instructions)
		return true, true
	default:
		return false, false
	}
}

func contentBlocksEndWithOptionalInstructions(blocks []any, instructions string) bool {
	if len(blocks) == 0 {
		return false
	}
	block, ok := blocks[len(blocks)-1].(map[string]any)
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
	merged := appendOptionalInstructions(existing, instructions)
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
	suffixBlock := map[string]any{"type": "text", "text": formatOptionalInstructions(instructions)}
	switch system := root["system"].(type) {
	case string:
		root["system"] = appendOptionalInstructions(system, instructions)
	case []any:
		if contentBlocksEndWithOptionalInstructions(system, instructions) {
			return body, false, nil
		}
		root["system"] = append(system, suffixBlock)
	case nil:
		root["system"] = []any{suffixBlock}
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
