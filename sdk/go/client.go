package lmbpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/tyro668/avora/internal/lmbp"
)

// Transport is an http.RoundTripper that serves requests in-process through
// the gateway handler (no network hop). Use it with any HTTP client,
// including the official openai-go / anthropic-sdk-go option.WithHTTPClient.
func (s *Server) Transport() http.RoundTripper { return inproc{s.handler} }

type inproc struct{ h http.Handler }

func (t inproc) RoundTrip(req *http.Request) (*http.Response, error) {
	// pre-call cancel: never start the handler (it would create a task)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	pr, pw := io.Pipe()
	w := &pipeWriter{hdr: http.Header{}, pw: pw, ready: make(chan struct{})}
	r := req.Clone(ctx)
	if r.Host == "" {
		r.Host = "in-process"
	}
	r.RemoteAddr = "127.0.0.1:0"
	r.RequestURI = r.URL.RequestURI()
	if r.Body == nil {
		r.Body = http.NoBody
	}
	failed := make(chan error, 1)
	go func() {
		defer cancel()
		defer func() {
			// producer failure: the consumer gets an error, never a hang or a
			// clean EOF that looks like a finished response
			if v := recover(); v != nil {
				err := fmt.Errorf("lmbp in-process handler failed: %v", v)
				failed <- err
				_ = pw.CloseWithError(err)
				return
			}
			w.finish()
		}()
		t.h.ServeHTTP(w, r)
	}()
	select {
	case <-w.ready:
	case err := <-failed:
		return nil, err
	case <-req.Context().Done():
		cancel()
		pr.Close()
		return nil, req.Context().Err()
	}
	// Body.Close (Stream.Close) cancels the handler's context, so the
	// gateway sees the caller leave and cancels the task.
	return &http.Response{StatusCode: w.code, Status: http.StatusText(w.code), Header: w.hdr, Body: &inprocBody{pr, cancel}, Request: req,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, ContentLength: -1}, nil
}

type inprocBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *inprocBody) Close() error { b.cancel(); return b.PipeReader.Close() }

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
	Code    string // lmbp error code (MODEL_NOT_FOUND, DEVICE_OFFLINE, ...)
	Message string
	Body    []byte
}

func (e *APIError) Error() string { return fmt.Sprintf("lmbp %d %s: %s", e.Status, e.Code, e.Message) }

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

// CallOption tunes one API call.
type CallOption func(*callOpts)

type callOpts struct{ idem, lastEventID string }

// WithIdempotencyKey sends Idempotency-Key: a retry with the same key and
// body re-attaches to the original task instead of running it again.
func WithIdempotencyKey(k string) CallOption { return func(o *callOpts) { o.idem = k } }

// WithLastEventID sends Last-Event-ID (use with WithIdempotencyKey on a
// stream to resume after Stream.LastEventID()).
func WithLastEventID(id string) CallOption { return func(o *callOpts) { o.lastEventID = id } }

func optsOf(opts []CallOption) callOpts {
	var o callOpts
	for _, f := range opts {
		f(&o)
	}
	return o
}

func (c *Client) do(ctx context.Context, method, path string, body any, o callOpts) (*http.Response, error) {
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
	if o.idem != "" {
		req.Header.Set("Idempotency-Key", o.idem)
	}
	if o.lastEventID != "" {
		req.Header.Set("Last-Event-ID", o.lastEventID)
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
				LMBPCode string `json:"lmbp_code"`
				Message  string `json:"message"`
			} `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(b, &e)
		code := e.Error.Code
		if code == "" {
			code = e.Error.LMBPCode
		}
		msg := e.Error.Message
		if msg == "" {
			msg = e.Detail
		}
		return nil, &APIError{Status: resp.StatusCode, Code: code, Message: msg, Body: b}
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, body any, o callOpts, out any) error {
	resp, err := c.do(ctx, method, path, body, o)
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
	DisplayName  string   `json:"display_name,omitempty"` // optional device-advertised name
	Capabilities []string `json:"capabilities"`
}

// ChatRequest is an OpenAI chat-completions request.
type ChatRequest struct {
	Model      string         `json:"model"`
	Messages   []lmbp.Message `json:"messages,omitempty"`
	Tools      []lmbp.Tool    `json:"tools,omitempty"`
	ToolChoice any            `json:"tool_choice,omitempty"`
	MaxTokens  int            `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the newer OpenAI alias of MaxTokens.
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	// Stop holds up to 4 stop sequences (the spec also allows one string).
	Stop []string `json:"stop,omitempty"`
	// N must be 1 when set (only one choice is supported).
	N      *int `json:"n,omitempty"`
	Stream bool `json:"stream"`
	// E2EE is a sealed request (E2EESealed.JWE); Messages/Tools must be empty.
	E2EE string `json:"lmbp_e2ee,omitempty"`
}

// E2EEFrame is one sealed response frame (stream chunk or aggregated list).
type E2EEFrame struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"` // task.delta | task.done
	Data string `json:"data"`
}

// ChatCompletion is a non-streaming response.
type ChatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int          `json:"index"`
		Message      lmbp.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *lmbp.Usage `json:"usage,omitempty"`
	E2EE  []E2EEFrame `json:"lmbp_e2ee,omitempty"` // sealed frames of an E2EE task
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
			ToolCalls []lmbp.ToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *lmbp.Usage `json:"usage,omitempty"`
	E2EE  *E2EEFrame  `json:"lmbp_e2ee,omitempty"` // sealed frame of an E2EE task
}

// Models lists the models on endUser's online devices.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	err := c.json(ctx, http.MethodGet, "/v1/models", nil, callOpts{}, &out)
	return out.Data, err
}

// ChatCompletion runs a non-streaming chat completion.
func (c *Client) ChatCompletion(ctx context.Context, req ChatRequest, opts ...CallOption) (*ChatCompletion, error) {
	req.Stream = false
	var out ChatCompletion
	if err := c.json(ctx, http.MethodPost, "/v1/chat/completions", req, optsOf(opts), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChatCompletionStream runs a streaming chat completion.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatRequest, opts ...CallOption) (*Stream[ChatCompletionChunk], error) {
	req.Stream = true
	resp, err := c.do(ctx, http.MethodPost, "/v1/chat/completions", req, optsOf(opts))
	if err != nil {
		return nil, err
	}
	return chatStream(resp), nil
}

// ResumeChatCompletionStream re-attaches to a task's stream after
// lastEventID (a Stream.LastEventID value; "" or "0" replays everything).
// It ends immediately when lastEventID is at or beyond the terminal event.
func (c *Client) ResumeChatCompletionStream(ctx context.Context, taskID, lastEventID string) (*Stream[ChatCompletionChunk], error) {
	resp, err := c.do(ctx, http.MethodGet, "/lmbp/v1/tenant/tasks/"+url.PathEscape(taskID)+"/events", nil, callOpts{lastEventID: lastEventID})
	if err != nil {
		return nil, err
	}
	return chatStream(resp), nil
}

func chatStream(resp *http.Response) *Stream[ChatCompletionChunk] {
	return newStream[ChatCompletionChunk](resp, func(_, data string) (sseAction, error) {
		if data == "[DONE]" {
			return sseEnd, nil
		}
		if strings.HasPrefix(data, `{"error"`) {
			return sseEnd, streamErr(data)
		}
		return sseDeliver, nil
	})
}

// ---------------- Anthropic-compatible ----------------

// MessagesRequest is an Anthropic Messages request. Content is a string or
// a slice of content blocks (maps or ContentBlock values).
type MessagesRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        any                `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream"`
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
func (c *Client) Messages(ctx context.Context, req MessagesRequest, opts ...CallOption) (*Message, error) {
	req.Stream = false
	var out Message
	if err := c.json(ctx, http.MethodPost, "/v1/messages", req, optsOf(opts), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MessagesStream runs a streaming Anthropic Messages request (ping events are skipped).
//
// To resume a dropped stream, call MessagesStream again with the same
// request, WithIdempotencyKey(sameKey) and WithLastEventID(stream.LastEventID()).
func (c *Client) MessagesStream(ctx context.Context, req MessagesRequest, opts ...CallOption) (*Stream[MessageStreamEvent], error) {
	req.Stream = true
	resp, err := c.do(ctx, http.MethodPost, "/v1/messages", req, optsOf(opts))
	if err != nil {
		return nil, err
	}
	return newStream[MessageStreamEvent](resp, func(event, data string) (sseAction, error) {
		switch event {
		case "ping":
			return sseSkip, nil
		case "error":
			return sseEnd, streamErr(data)
		case "message_stop":
			return sseDeliverEnd, nil
		}
		return sseDeliver, nil
	}), nil
}

// ---------------- streams ----------------

// ErrIncompleteStream reports a stream that ended (EOF or a broken
// connection) before its terminator: data: [DONE] for chat completions,
// message_stop for Messages, or an error event. The response is NOT
// complete. errors.As the error to an *IncompleteStreamError for the resume
// cursor: re-attach with ResumeChatCompletionStream(TaskID, LastEventID), or
// repeat the request with WithIdempotencyKey(sameKey) and
// WithLastEventID(LastEventID).
var ErrIncompleteStream = errors.New("lmbp: stream ended before its terminator")

// IncompleteStreamError is the concrete ErrIncompleteStream.
type IncompleteStreamError struct {
	TaskID      string // X-Task-Id of the stream ("" if the gateway sent none)
	LastEventID string // id of the last fully received event: the resume cursor
	Cause       error  // the read error, or nil for a clean EOF
}

func (e *IncompleteStreamError) Error() string {
	m := fmt.Sprintf("%v (task %q, last event id %q)", ErrIncompleteStream, e.TaskID, e.LastEventID)
	if e.Cause != nil {
		m += ": " + e.Cause.Error()
	}
	return m
}
func (e *IncompleteStreamError) Is(target error) bool { return target == ErrIncompleteStream }
func (e *IncompleteStreamError) Unwrap() error        { return e.Cause }

type sseAction int

const (
	sseDeliver    sseAction = iota // decode and return the event
	sseSkip                        // ignore it (ping)
	sseEnd                         // terminator: stop without delivering (with err for error events)
	sseDeliverEnd                  // deliver it, and it is the terminator (message_stop)
)

// Stream iterates server-sent events: for s.Next() { s.Current() }; then
// s.Err(). Err is nil only when the stream reached its terminator; a
// truncated stream yields an *IncompleteStreamError (ErrIncompleteStream).
type Stream[T any] struct {
	body       io.ReadCloser
	taskID     string
	lastID     string
	sc         *bufio.Scanner
	check      func(event, data string) (sseAction, error)
	cur        T
	err        error
	done       bool
	terminated bool
}

func newStream[T any](resp *http.Response, check func(event, data string) (sseAction, error)) *Stream[T] {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	return &Stream[T]{body: resp.Body, taskID: resp.Header.Get("X-Task-Id"), sc: sc, check: check}
}

// TaskID is the gateway task behind the stream (X-Task-Id), for
// ResumeChatCompletionStream.
func (s *Stream[T]) TaskID() string { return s.taskID }

// LastEventID is the SSE id of the last event read by Next: the resume
// cursor to pass as Last-Event-ID. Events without an id keep the previous one.
func (s *Stream[T]) LastEventID() string { return s.lastID }

// Next advances to the next event.
func (s *Stream[T]) Next() bool {
	for !s.done {
		if s.terminated {
			s.done = true
			break
		}
		event, data, id := "", "", ""
		dispatched := false
		for s.sc.Scan() {
			l := s.sc.Text()
			if l == "" {
				if data != "" {
					dispatched = true
					break
				}
				event, id = "", ""
				continue
			}
			if v, ok := strings.CutPrefix(l, "event: "); ok {
				event = v
			} else if v, ok := strings.CutPrefix(l, "id: "); ok {
				id = v
			} else if v, ok := strings.CutPrefix(l, "data: "); ok {
				data += v
			}
		}
		if !dispatched {
			// EOF or read error before the terminator. A trailing event
			// without its blank line is incomplete and is not delivered.
			s.err, s.done = &IncompleteStreamError{TaskID: s.taskID, LastEventID: s.lastID, Cause: s.sc.Err()}, true
			break
		}
		act, err := s.check(event, data)
		if act == sseSkip {
			continue
		}
		if act == sseEnd {
			s.err, s.done = err, true
			break
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			s.err, s.done = err, true
			break
		}
		s.cur = v
		if id != "" {
			s.lastID = id
		}
		if act == sseDeliverEnd {
			s.terminated = true
		}
		return true
	}
	s.body.Close()
	return false
}

// Current is the event read by the last Next.
func (s *Stream[T]) Current() T { return s.cur }

// Err is the error that stopped the stream: nil after the terminator,
// *IncompleteStreamError after a truncation, *APIError after an error event.
func (s *Stream[T]) Err() error { return s.err }

// Close abandons the stream (the gateway cancels the task).
func (s *Stream[T]) Close() error { s.done = true; return s.body.Close() }

func streamErr(data string) error {
	var e struct {
		Error struct {
			Code     string `json:"code"`
			LMBPCode string `json:"lmbp_code"`
			Message  string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(data), &e)
	code := e.Error.Code
	if code == "" {
		code = e.Error.LMBPCode
	}
	return &APIError{Status: 502, Code: code, Message: e.Error.Message, Body: []byte(data)}
}
