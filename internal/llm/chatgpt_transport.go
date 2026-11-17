package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

type chatGPTTransport struct {
	base  http.RoundTripper
	token func(context.Context) (string, error)
}

func (t *chatGPTTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	owner, _ := original.Context().Value(responseBodyKey{}).(*responseBody)
	call, _ := original.Context().Value(chatGPTCallKey{}).(*chatGPTCall)
	req, err := t.prepareRequest(original, call)
	if err != nil {
		return nil, owner.recordFailure(err)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, owner.recordFailure(readErr)
		}
		// Keep the original body shape, status, and request ID for diagnostics,
		// but never retain an echoed bearer credential in a diagnostic.
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		data = bytes.ReplaceAll(data, []byte(token), []byte("[redacted]"))
		resp.Body = io.NopCloser(bytes.NewReader(data))
		owner.recordFailure(newChatGPTError(resp.StatusCode, resp.Header.Get("X-Request-Id"), data))
		return resp, nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		_ = resp.Body.Close()
		return nil, owner.recordFailure(errors.New("ChatGPT returned a non-streaming response; no tools were executed"))
	}
	resp.Body = &chatGPTStreamBody{
		call: call, body: resp.Body, reader: bufio.NewReader(resp.Body), fail: owner.recordFailure,
		requestID: resp.Header.Get("X-Request-Id"),
	}
	return resp, nil
}

func (t *chatGPTTransport) prepareRequest(original *http.Request, call *chatGPTCall) (*http.Request, error) {
	if original.Body != nil {
		defer original.Body.Close()
	}
	// Check the destination and the complete replayed input before fetching a token.
	if original.Method != http.MethodPost || original.URL.String() != chatGPTBaseURL+"/responses" {
		return nil, errors.New("chatgpt permits only POST https://api.openai.com/v1/responses")
	}
	if call == nil || original.Body == nil {
		return nil, errors.New("ChatGPT request is missing its conversation history")
	}
	const maxBody = defaultInputByteCeiling * 4
	data, err := io.ReadAll(io.LimitReader(original.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, errors.New("ChatGPT request exceeds the bounded input size")
	}
	body, err := encodeChatGPTRequest(data, call.prompt)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("ChatGPT request exceeds the bounded input size")
	}
	token, err := t.token(original.Context())
	if errors.Is(err, chatgpt.ErrAuth) {
		return nil, fmt.Errorf("%w: %w", ErrAuth, err)
	}
	if err != nil {
		return nil, fmt.Errorf("ChatGPT credential: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: no ChatGPT access token; run roast chatgpt signin", ErrAuth)
	}
	req := original.Clone(original.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("OpenAI-Organization")
	req.Header.Del("OpenAI-Project")
	return req, nil
}
