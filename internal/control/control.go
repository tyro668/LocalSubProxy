// Package control is the local-only control channel of `avora
// serve`: newline-delimited JSON requests, each one a native API action
// (the same payload as AVORA_Call), answered with the native reply envelope.
//
// The channel is a unix socket (mode 0600) in the data directory on Linux
// and macOS, and a named pipe that only the current user may open on
// Windows. Nothing listens on TCP.
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxRequest bounds one request line.
const MaxRequest = 1 << 20

// Handler answers one request (raw JSON) with a reply (raw JSON).
type Handler func(req string) string

// refused lists actions the channel never forwards, with the reason.
var refused = map[string]string{
	"stop": `stopping the kernel over the control channel is not supported; stop "avora serve" itself (Ctrl-C, SIGTERM or the service manager)`,
}

func reply(err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(b)
}

// Serve accepts connections on l until ctx ends. Each connection may send
// several requests; they are handled one at a time in order.
func Serve(ctx context.Context, l net.Listener, h Handler) error {
	go func() { <-ctx.Done(); _ = l.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(ctx, c, h)
		}()
	}
}

func serveConn(ctx context.Context, c net.Conn, h Handler) {
	defer c.Close()
	go func() { <-ctx.Done(); _ = c.Close() }()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), MaxRequest)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		out := handle(line, h)
		if _, err := io.WriteString(c, out+"\n"); err != nil {
			return
		}
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		_, _ = io.WriteString(c, reply(fmt.Errorf("request larger than %d bytes", MaxRequest))+"\n")
	}
}

func handle(line string, h Handler) string {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return reply(fmt.Errorf("invalid request: %w", err))
	}
	if req.Action == "" {
		return reply(errors.New("action required"))
	}
	if why, ok := refused[req.Action]; ok {
		return reply(errors.New(why))
	}
	return h(line)
}

// Client sends requests to a running serve.
type Client struct {
	c  net.Conn
	rd *bufio.Reader
}

// ErrNotRunning means nothing listens on the data directory's channel.
var ErrNotRunning = errors.New(`"avora serve" is not running for this data directory`)

// Connect opens the control channel of the serve that owns dataDir.
func Connect(dataDir string) (*Client, error) {
	c, err := dial(dataDir, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %v", ErrNotRunning, Address(dataDir), err)
	}
	return &Client{c: c, rd: bufio.NewReaderSize(c, 64<<10)}, nil
}

// Do sends one request and returns the raw reply.
func (cl *Client) Do(req any) (json.RawMessage, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := cl.c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := cl.rd.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("control channel: %w", err)
	}
	return json.RawMessage(line), nil
}

// Close closes the connection.
func (cl *Client) Close() error { return cl.c.Close() }
