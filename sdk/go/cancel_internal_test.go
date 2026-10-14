package lmbpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A4: the in-process transport must handle pre-call cancel, mid-call
// cancel, a handler failure after headers and an abandoned body the same
// way: the caller gets an error (never a hang), and the handler's
// context is cancelled so the gateway stops the task.

func get(t *testing.T, ctx context.Context, h http.Handler) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://in-process/x", nil)
	return inproc{h}.RoundTrip(req)
}

func waitCh(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestInprocPreCancelledNeverRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := make(chan struct{}, 1)
	_, err := get(t, ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran <- struct{}{} }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(ran) != 0 {
		t.Fatal("handler ran for a request cancelled before the call (a task would be created)")
	}
}

// streaming handler: headers, one chunk, then wait for its context
func streamer(exited chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(exited)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: 1\n\n"))
		<-r.Context().Done()
	})
}

func TestInprocMidCallCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	resp, err := get(t, ctx, streamer(exited))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitCh(t, exited, "handler exit after cancel")
	done := make(chan struct{})
	go func() { _, _ = io.ReadAll(resp.Body); close(done) }()
	waitCh(t, done, "body EOF after cancel")
}

func TestInprocBodyCloseCancelsHandler(t *testing.T) {
	exited := make(chan struct{})
	resp, err := get(t, context.Background(), streamer(exited))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	waitCh(t, exited, "handler context cancelled by Body.Close")
}

func TestInprocHandlerPanicAfterHeaders(t *testing.T) {
	resp, err := get(t, context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: 1\n\n"))
		panic("producer died")
	}))
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := io.ReadAll(resp.Body); ch <- res{b, err} }()
	select {
	case r := <-ch:
		if r.err == nil || !strings.Contains(r.err.Error(), "producer died") {
			t.Fatalf("read after producer panic: %q err %v (want an error, not a clean EOF)", r.b, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer hung after the producer died")
	}
}

func TestInprocHandlerPanicBeforeHeaders(t *testing.T) {
	_, err := get(t, context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v, want the handler failure", err)
	}
}
