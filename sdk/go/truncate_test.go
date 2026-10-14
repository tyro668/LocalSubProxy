package lmbpserver

// Third audit item 3: a stream that hits EOF before its terminator
// ([DONE] / message_stop / an error event) is not a normal end. The client
// must report ErrIncompleteStream carrying the resume cursor.

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func fakeClient(body string) *Client {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Task-Id", "task_fake")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body)) // then the connection just ends
	})
	return &Client{hc: &http.Client{Transport: inproc{h}}, key: "k", user: "u", ten: "t"}
}

const chunkA = `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"A"},"finish_reason":null}]}`

func drainChat(t *testing.T, body string) (string, *Stream[ChatCompletionChunk]) {
	t.Helper()
	s, err := fakeClient(body).ChatCompletionStream(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for s.Next() {
		for _, c := range s.Current().Choices {
			text += c.Delta.Content
		}
	}
	return text, s
}

func wantIncomplete(t *testing.T, err error, cursor string) {
	t.Helper()
	var ie *IncompleteStreamError
	if !errors.Is(err, ErrIncompleteStream) || !errors.As(err, &ie) {
		t.Fatalf("Err() = %v, want ErrIncompleteStream", err)
	}
	if ie.LastEventID != cursor || ie.TaskID != "task_fake" {
		t.Fatalf("cursor %q task %q, want %q task_fake", ie.LastEventID, ie.TaskID, cursor)
	}
}

func TestTruncatedChatStream(t *testing.T) {
	cases := []struct{ name, body, cursor, text string }{
		{"eof after two chunks", "id: 1\ndata: " + chunkA + "\n\nid: 2\ndata: " + chunkA + "\n\n", "2", "AA"},
		{"eof mid-event is not delivered", "id: 1\ndata: " + chunkA + "\n\nid: 2\ndata: " + chunkA, "1", "A"},
		{"eof before any event", "", "", ""},
		{"eof after finish chunk but before [DONE]", "id: 1\ndata: " + chunkA + "\n\nid: 2\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", "2", "A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, s := drainChat(t, c.body)
			if text != c.text {
				t.Fatalf("text %q want %q", text, c.text)
			}
			wantIncomplete(t, s.Err(), c.cursor)
			if s.LastEventID() != c.cursor {
				t.Fatalf("LastEventID %q", s.LastEventID())
			}
		})
	}
	t.Run("[DONE] is a normal end", func(t *testing.T) {
		_, s := drainChat(t, "id: 1\ndata: "+chunkA+"\n\ndata: [DONE]\n\n")
		if s.Err() != nil {
			t.Fatal(s.Err())
		}
	})
	t.Run("error event is an API error, not incomplete", func(t *testing.T) {
		_, s := drainChat(t, "id: 1\ndata: {\"error\":{\"code\":\"DEVICE_OFFLINE\",\"message\":\"x\"}}\n\n")
		var ae *APIError
		if !errors.As(s.Err(), &ae) || errors.Is(s.Err(), ErrIncompleteStream) {
			t.Fatalf("Err() = %v", s.Err())
		}
	})
}

func TestTruncatedMessagesStream(t *testing.T) {
	head := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n" +
		"event: content_block_delta\nid: 1\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"A\"}}\n\n"
	run := func(body string) *Stream[MessageStreamEvent] {
		s, err := fakeClient(body).MessagesStream(context.Background(), MessagesRequest{Model: "m", MaxTokens: 8})
		if err != nil {
			t.Fatal(err)
		}
		for s.Next() {
		}
		return s
	}
	t.Run("eof before message_stop", func(t *testing.T) {
		wantIncomplete(t, run(head+"event: ping\ndata: {\"type\":\"ping\"}\n\n").Err(), "1")
	})
	t.Run("message_stop is a normal end", func(t *testing.T) {
		s := run(head + "event: message_delta\nid: 2\ndata: {\"type\":\"message_delta\",\"delta\":{}}\n\nevent: message_stop\nid: 2\ndata: {\"type\":\"message_stop\"}\n\n")
		if s.Err() != nil {
			t.Fatal(s.Err())
		}
	})
}
