package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The preview does not accept ordinary Responses defaults. Keep this boundary
// explicit so SDK updates cannot accidentally enable storage or API billing.
func chatGPTRequest(body []byte) ([]byte, error) {
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
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(request["input"], &input); err != nil || input == nil {
		return nil, errors.New("ChatGPT input must be an array containing the conversation history")
	}
	for _, item := range input {
		if string(item["role"]) == `"system"` {
			item["role"] = json.RawMessage(`"developer"`)
		}
		if string(item["type"]) == `"function_call"` {
			// Fantasy currently drops the namespace from function-call history.
			// All calls admitted by the stream guard belong to this namespace.
			item["namespace"] = json.RawMessage(`"roast"`)
		}
	}
	request["input"], _ = json.Marshal(input)
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
