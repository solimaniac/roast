package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Validate complete SSE frames before passing their unchanged bytes to the
// SDK. In particular, response.incomplete must never masquerade as a successful
// tool submission even if an SDK maps its missing finish reason to "stop".
type chatGPTStreamBody struct {
	body      io.ReadCloser
	reader    *bufio.Reader
	pending   []byte
	completed bool
	err       error
	fail      func(error) error
	requestID string
	call      *chatGPTCall
}

func (b *chatGPTStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 && b.err == nil {
		b.pending, b.err = b.readEvent()
		if b.err != nil && !errors.Is(b.err, io.EOF) {
			// The SDK may stop after reading a terminal frame. Record a failed
			// validation before returning even the first byte of that frame.
			b.fail(b.err)
		}
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	return 0, b.err
}

// readEvent keeps the original SSE bytes for capture. Only data fields are
// joined for validation. Comments and line endings pass through unchanged.
func (b *chatGPTStreamBody) readEvent() ([]byte, error) {
	var frame bytes.Buffer
	var data []string
	for {
		line, err := b.readLine()
		if frame.Len()+len(line) > maxCapturedResponseSize {
			return frame.Bytes(), errors.New("ChatGPT stream event exceeds the bounded size")
		}
		frame.WriteString(line)
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
		if line != "" && err == nil {
			continue
		}
		if len(data) > 0 {
			if invalid := b.validate([]byte(strings.Join(data, "\n"))); invalid != nil {
				return frame.Bytes(), invalid
			}
		}
		if errors.Is(err, io.EOF) && !b.completed {
			return frame.Bytes(), errors.New("ChatGPT stream ended without response.completed; no tools were executed")
		}
		return frame.Bytes(), err
	}
}

// ReadSlice bounds allocation even when a peer never sends a newline.
func (b *chatGPTStreamBody) readLine() (string, error) {
	var line bytes.Buffer
	for {
		fragment, err := b.reader.ReadSlice('\n')
		if line.Len()+len(fragment) > maxCapturedResponseSize {
			return "", errors.New("ChatGPT stream line exceeds the bounded size")
		}
		line.Write(fragment)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line.String(), err
		}
	}
}

func (b *chatGPTStreamBody) Close() error { return b.body.Close() }

func (b *chatGPTStreamBody) validate(data []byte) error {
	if bytes.Equal(data, []byte("[DONE]")) {
		if !b.completed {
			return errors.New("ChatGPT stream ended before response.completed")
		}
		return nil
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Status string            `json:"status"`
			Error  json.RawMessage   `json:"error"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("invalid ChatGPT stream event: %w", err)
	}
	if len(event.Item) > 0 {
		if err := validateChatGPTOutputItem(event.Item); err != nil {
			return err
		}
	}
	for _, item := range event.Response.Output {
		if err := validateChatGPTOutputItem(item); err != nil {
			return err
		}
	}
	switch event.Type {
	case "response.completed":
		if event.Response.Status != "completed" {
			return errors.New("ChatGPT completion event has a non-completed status")
		}
		b.completed = true
		if b.call != nil {
			b.call.output.items = event.Response.Output
		}
	case "response.incomplete":
		return errors.New("ChatGPT response was incomplete; no tools were executed (max_output_tokens is unsupported on this route)")
	case "response.failed":
		return newChatGPTError(http.StatusOK, b.requestID, event.Response.Error)
	case "error":
		return newChatGPTError(http.StatusOK, b.requestID, data)
	}
	return nil
}

func validateChatGPTOutputItem(raw json.RawMessage) error {
	var item struct {
		Type      string `json:"type"`
		Role      string `json:"role"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	switch item.Type {
	case "function_call":
		if item.Namespace != chatGPTToolNamespace {
			return errors.New("ChatGPT returned a function outside the Roast tool namespace; no tools were executed")
		}
	case "message":
		if item.Role != "assistant" {
			return errors.New("ChatGPT returned an output message with an invalid role; no tools were executed")
		}
	case "reasoning":
	default:
		return fmt.Errorf("ChatGPT returned unsupported output type %q; no tools were executed", item.Type)
	}
	return nil
}
