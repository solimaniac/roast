package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

const chatGPTOutputKey = "roast_chatgpt_output"

type chatGPTCallKey struct{}
type chatGPTCall struct {
	prompt fantasy.Prompt
	output *chatGPTOutput
}

// Finalized raw output survives Fantasy's conversion to prompt messages. Its
// Responses adapter otherwise discards reasoning and namespace metadata. One
// shared marker on the turn's content restores the complete output exactly
// once, in server order, without promoting it to a higher-priority message.
type chatGPTOutput struct {
	Items []json.RawMessage `json:"items"`
}

func (*chatGPTOutput) Options()                          {}
func (o *chatGPTOutput) MarshalJSON() ([]byte, error)    { return json.Marshal(o.Items) }
func (o *chatGPTOutput) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &o.Items) }

type chatGPTModel struct{ fantasy.LanguageModel }

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	state := &chatGPTCall{prompt: call.Prompt, output: &chatGPTOutput{}}
	ctx = context.WithValue(ctx, chatGPTCallKey{}, state)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeToolCall, fantasy.StreamPartTypeTextEnd, fantasy.StreamPartTypeReasoningEnd:
				metadata := make(fantasy.ProviderMetadata, len(part.ProviderMetadata)+1)
				for key, value := range part.ProviderMetadata {
					metadata[key] = value
				}
				metadata[chatGPTOutputKey] = state.output
				part.ProviderMetadata = metadata
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func chatGPTPromptRequest(body []byte, prompt fantasy.Prompt) ([]byte, error) {
	input := make([]json.RawMessage, 0, len(prompt))
	appendItem := func(value any) error {
		raw, err := json.Marshal(value)
		if err == nil {
			input = append(input, raw)
		}
		return err
	}
	for _, message := range prompt {
		seen := make(map[*chatGPTOutput]bool)
		for _, part := range message.Content {
			if original, ok := part.Options()[chatGPTOutputKey].(*chatGPTOutput); ok {
				if message.Role != fantasy.MessageRoleAssistant || len(original.Items) == 0 {
					return nil, errors.New("ChatGPT history is missing finalized assistant output")
				}
				if !seen[original] {
					input = append(input, original.Items...)
					seen[original] = true
				}
				continue
			}
			var item any
			switch part.GetType() {
			case fantasy.ContentTypeText:
				text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
				if !ok {
					return nil, errors.New("invalid ChatGPT text history")
				}
				role := string(message.Role)
				if message.Role == fantasy.MessageRoleSystem {
					role = "developer"
				}
				item = map[string]any{"role": role, "content": text.Text}
			case fantasy.ContentTypeToolCall:
				tool, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
				if !ok || tool.ProviderExecuted {
					return nil, errors.New("invalid ChatGPT local tool history")
				}
				item = map[string]any{"type": "function_call", "call_id": tool.ToolCallID, "name": tool.ToolName, "namespace": chatGPTToolNamespace, "arguments": tool.Input}
			case fantasy.ContentTypeToolResult:
				tool, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok || tool.ProviderExecuted {
					return nil, errors.New("invalid ChatGPT local tool result")
				}
				var output string
				if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tool.Output); ok {
					output = text.Text
				} else if failure, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tool.Output); ok && failure.Error != nil {
					output = failure.Error.Error()
				} else {
					return nil, errors.New("ChatGPT Roast supports text tool results only")
				}
				item = map[string]any{"type": "function_call_output", "call_id": tool.ToolCallID, "output": output}
			default:
				return nil, fmt.Errorf("ChatGPT Roast cannot replay unrecognized history type %q", part.GetType())
			}
			if err := appendItem(item); err != nil {
				return nil, err
			}
		}
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("invalid ChatGPT request object")
	}
	request["input"], _ = json.Marshal(input)
	return json.Marshal(request)
}
