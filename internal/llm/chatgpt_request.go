package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

// The preview does not accept ordinary Responses defaults. Keep this boundary
// explicit so SDK updates cannot accidentally enable storage or API billing.
func encodeChatGPTRequest(body []byte, prompt fantasy.Prompt) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode ChatGPT request: %w", err)
	}
	if request == nil {
		return nil, errors.New("ChatGPT request must be an object")
	}
	request["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)
	for _, key := range []string{
		"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt",
		"prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id",
	} {
		delete(request, key)
	}
	request["store"], request["stream"] = json.RawMessage("false"), json.RawMessage("true")
	if input := request["input"]; len(input) == 0 || input[0] != '[' {
		return nil, errors.New("ChatGPT input must be an array containing the conversation history")
	}
	// Fantasy drops finalized reasoning and tool namespaces while building its
	// input. Use the prompt captured by chatGPTModel for every turn.
	input, err := chatGPTInput(prompt)
	if err != nil {
		return nil, err
	}
	request["input"] = input
	var tools []map[string]json.RawMessage
	if raw := request["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode ChatGPT tools: %w", err)
		}
	}
	for _, tool := range tools {
		if string(tool["type"]) != `"function"` {
			return nil, errors.New("ChatGPT Roast supports only local function tools")
		}
	}
	if len(tools) > 0 {
		request["tools"], _ = json.Marshal([]any{map[string]any{
			"type": "namespace", "name": chatGPTToolNamespace,
			"description": "Roast code review and final submission tools.", "tools": tools,
		}})
	}
	return json.Marshal(request)
}
