package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"charm.land/fantasy"
)

const chatGPTOutputKey = "roast_chatgpt_output"

type chatGPTCallKey struct{}
type chatGPTCall struct {
	prompt fantasy.Prompt
	output chatGPTOutput
}

// Finalized raw output survives Fantasy's conversion to prompt messages. Its
// Responses adapter otherwise discards reasoning and namespace metadata. One
// shared marker on the turn's content restores the complete output exactly
// once, in server order, without promoting it to a higher-priority message.
type chatGPTOutput struct {
	items []json.RawMessage
}

func (*chatGPTOutput) Options()                          {}
func (o *chatGPTOutput) MarshalJSON() ([]byte, error)    { return json.Marshal(o.items) }
func (o *chatGPTOutput) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &o.items) }

type chatGPTModel struct{ fantasy.LanguageModel }

func (m *chatGPTModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	state := &chatGPTCall{prompt: call.Prompt}
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
				maps.Copy(metadata, part.ProviderMetadata)
				metadata[chatGPTOutputKey] = &state.output
				part.ProviderMetadata = metadata
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// chatGPTInput encodes the full conversation for a stateless Responses call.
func chatGPTInput(prompt fantasy.Prompt) (json.RawMessage, error) {
	input := make([]any, 0, len(prompt))
	for _, message := range prompt {
		seen := make(map[*chatGPTOutput]bool)
		for _, part := range message.Content {
			if original, ok := part.Options()[chatGPTOutputKey].(*chatGPTOutput); ok {
				if message.Role != fantasy.MessageRoleAssistant || original == nil || len(original.items) == 0 {
					return nil, errors.New("ChatGPT history is missing finalized assistant output")
				}
				if !seen[original] {
					for _, item := range original.items {
						input = append(input, item)
					}
					seen[original] = true
				}
				continue
			}
			item, err := chatGPTInputPart(message.Role, part)
			if err != nil {
				return nil, err
			}
			input = append(input, item)
		}
	}
	return json.Marshal(input)
}

func chatGPTInputPart(role fantasy.MessageRole, part fantasy.MessagePart) (map[string]string, error) {
	switch part.GetType() {
	case fantasy.ContentTypeText:
		text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
		if !ok {
			return nil, errors.New("invalid ChatGPT text history")
		}
		if role == fantasy.MessageRoleSystem {
			role = "developer"
		}
		return map[string]string{"role": string(role), "content": text.Text}, nil
	case fantasy.ContentTypeToolCall:
		tool, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
		if !ok || tool.ProviderExecuted {
			return nil, errors.New("invalid ChatGPT local tool history")
		}
		return map[string]string{
			"type": "function_call", "call_id": tool.ToolCallID,
			"namespace": chatGPTToolNamespace, "name": tool.ToolName, "arguments": tool.Input,
		}, nil
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
		return map[string]string{"type": "function_call_output", "call_id": tool.ToolCallID, "output": output}, nil
	default:
		return nil, fmt.Errorf("ChatGPT Roast cannot replay unrecognized history type %q", part.GetType())
	}
}
