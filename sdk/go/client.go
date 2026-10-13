package lsp2server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"localsubproxy/internal/lsp2"
)

// Transport is an http.RoundTripper that serves requests in-process through
// the gateway handler (no network hop). Use it with any HTTP client,
// including the official openai-go / anthropic-sdk-go option.WithHTTPClient.
func (s *Server) Transport() http.RoundTripper { return inproc{s.handler} }

type inproc struct{ h http.Handler }

func (t inproc) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &pipeWriter{hdr: http.Header{}, pw: pw, ready: make(chan struct{})}
	r := req.Clone(req.Context())
	if r.Host == "" {
		r.Host = "in-process"
	}
	r.RemoteAddr = "127.0.0.1:0"
	r.RequestURI = r.URL.RequestURI()
	if r.Body == nil {
		r.Body = http.NoBody
	}
	go func() {
		defer w.finish()
		t.h.ServeHTTP(w, r)
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		pr.Close()
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: w.code, Status: http.StatusText(w.code), Header: w.hdr, Body: pr, Request: req,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, ContentLength: -1}, nil
}

type pipeWriter struct {
	hdr   http.Header
	pw    *io.PipeWriter
	code  int
	once  sync.Once
	ready chan struct{}
}

func (w *pipeWriter) Header() http.Header { return w.hdr }
func (w *pipeWriter) WriteHeader(code int) {
	w.once.Do(func() { w.code = code; close(w.ready) })
}
func (w *pipeWriter) Write(b []byte) (int, error) { w.WriteHeader(200); return w.pw.Write(b) }
func (w *pipeWriter) Flush()                      {}
func (w *pipeWriter) finish()                     { w.WriteHeader(200); w.pw.Close() }

// APIError is a non-2xx response from the gateway.
type APIError struct {
	Status  int
	Code    string // lsp2 error code (MODEL_NOT_FOUND, DEVICE_OFFLINE, ...)
	Message string
	Body    []byte
}

func (e *APIError) Error() string { return fmt.Sprintf("lsp2 %d %s: %s", e.Status, e.Code, e.Message) }

// Client calls the gateway in-process as one end user of one tenant.
type Client struct {
	hc             *http.Client
	key, user, ten string
}

// Client returns an in-process client for endUser of tenant.
func (s *Server) Client(tenant, endUser string) (*Client, error) {
	k, ok := s.keys[tenant]
	if !ok {
		return nil, ErrUnknownTenant
	}
	return &Client{hc: &http.Client{Transport: s.Transport()}, key: k, user: endUser, ten: tenant}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, idem string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://in-process"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-End-User", c.user)
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var e struct {
			Error struct {
				Code     string `json:"code"`
				LSP2Code string `json:"lsp2_code"`
				Message  string `json:"message"`
			} `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(b, &e)
		code := e.Error.Code
		if code == "" {
			code = e.Error.LSP2Code
		}
		msg := e.Error.Message
		if msg == "" {
			msg = e.Detail
		}
		return nil, &APIError{Status: resp.StatusCode, Code: code, Message: msg, Body: b}
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, body any, idem string, out any) error {
	resp, err := c.do(ctx, method, path, body, idem)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------------- OpenAI-compatible ----------------

// Model is one entry of GET /v1/models.
type Model struct {
	ID           string   `json:"id"`
	Object       string   `json:"object"`
	OwnedBy      string   `json:"owned_by"`
	Capabilities []string `json:"capabilities"`
}

// ChatRequest is an OpenAI chat-completions request.
type ChatRequest struct {
	Model       string         `json:"model"`
	Messages    []lsp2.Message `json:"messages"`
	Tools       []lsp2.Tool    `json:"tools,omitempty"`
	ToolChoice  any            `json:"tool_choice,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Stream      bool           `json:"stream"`
}

// ChatCompletion is a non-streaming response.
type ChatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int          `json:"index"`
		Message      lsp2.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *lsp2.Usage `json:"usage,omitempty"`
}

// ChatCompletionChunk is one streamed event.
type ChatCompletionChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string          `json:"role,omitempty"`
			Content   string          `json:"content,omitempty"`
			ToolCalls []lsp2.ToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *lsp2.Usage `json:"usage,omitempty"`
}

// Models lists the models on endUser's online devices.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	err := c.json(ctx, http.MethodGet, "/v1/models", nil, "", &out)
	return out.Data, err
}

// ChatCompletion runs a non-streaming chat completion.
func (c *Client) ChatCompletion(ctx context.Context, req ChatRequest) (*ChatCompletion, error) {
	req.Stream = false
	var out ChatCompletion
	if err := c.json(ctx, http.MethodPost, "/v1/chat/completions", req, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChatCompletionStream runs a streaming chat completion.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatRequest) (*Stream[ChatCompletionChunk], error) {
	req.Stream = true
	resp, err := c.do(ctx, http.MethodPost, "/v1/chat/completions", req, "")
	if err != nil {
		return nil, err
	}
	return newStream[ChatCompletionChunk](resp.Body, func(_, data string) (bool, error) {
		if data == "[DONE]" {
			return true, nil
		}
		if strings.HasPrefix(data, `{"error"`) {
			return true, streamErr(data)
		}
		return false, nil
	}), nil
}

// ---------------- Anthropic-compatible ----------------

// MessagesRequest is an Anthropic Messages request. Content is a string or
// a slice of content blocks (maps or ContentBlock values).
type MessagesRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      any                `json:"system,omitempty"`
	Messages    []AnthropicMessage `json:"messages"`
	Tools       []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type AnthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

// ContentBlock is a text or tool_use block of a Message.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Message is a non-streaming Anthropic response.
type Message struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// MessageStreamEvent is one named SSE event of a streamed message.
type MessageStreamEvent struct {
	Type         string        `json:"type"`
	Index        int           `json:"index"`
	Message      *Message      `json:"message,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        struct {
		Type        string `json:"type,omitempty"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// Messages runs a non-streaming Anthropic Messages request.
func (c *Client) Messages(ctx context.Context, req MessagesRequest) (*Message, error) {
	req.Stream = false
	var out Message
	if err := c.json(ctx, http.MethodPost, "/v1/messages", req, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MessagesStream runs a streaming Anthropic Messages request (ping events are skipped).
func (c *Client) MessagesStream(ctx context.Context, req MessagesRequest) (*Stream[MessageStreamEvent], error) {
	req.Stream = true
	resp, err := c.do(ctx, http.MethodPost, "/v1/messages", req, "")
	if err != nil {
		return nil, err
	}
	return newStream[MessageStreamEvent](resp.Body, func(event, data string) (bool, error) {
		switch event {
		case "ping":
			return false, errSkip
		case "error":
			return true, streamErr(data)
		}
		return false, nil
	}), nil
}

// ---------------- streams ----------------

var errSkip = errors.New("skip")

// Stream iterates server-sent events: for s.Next() { s.Current() }; then s.Err().
type Stream[T any] struct {
	body  io.ReadCloser
	sc    *bufio.Scanner
	check func(event, data string) (stop bool, err error)
	cur   T
	err   error
	done  bool
}

func newStream[T any](body io.ReadCloser, check func(event, data string) (bool, error)) *Stream[T] {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	return &Stream[T]{body: body, sc: sc, check: check}
}

// Next advances to the next event.
func (s *Stream[T]) Next() bool {
	for !s.done {
		event, data := "", ""
		for s.sc.Scan() {
			l := s.sc.Text()
			if l == "" {
				if data != "" {
					break
				}
				continue
			}
			if v, ok := strings.CutPrefix(l, "event: "); ok {
				event = v
			} else if v, ok := strings.CutPrefix(l, "data: "); ok {
				data += v
			}
		}
		if data == "" {
			s.err, s.done = s.sc.Err(), true
			break
		}
		stop, err := s.check(event, data)
		if errors.Is(err, errSkip) {
			continue
		}
		if stop {
			s.err, s.done = err, true
			break
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			s.err, s.done = err, true
			break
		}
		s.cur = v
		return true
	}
	s.body.Close()
	return false
}

// Current is the event read by the last Next.
func (s *Stream[T]) Current() T { return s.cur }

// Err is the error that stopped the stream, if any.
func (s *Stream[T]) Err() error { return s.err }

// Close abandons the stream (the gateway cancels the task).
func (s *Stream[T]) Close() error { s.done = true; return s.body.Close() }

func streamErr(data string) error {
	var e struct {
		Error struct {
			Code     string `json:"code"`
			LSP2Code string `json:"lsp2_code"`
			Message  string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	code := e.Error.Code
	if code == "" {
		code = e.Error.LSP2Code
	}
	return &APIError{Status: 502, Code: code, Message: e.Error.Message, Body: []byte(data)}
}
