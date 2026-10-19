package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tyro668/avora/internal/backend"
)

// CodexLogin keeps the official App Server alive for its browser callback.
// Credentials remain in the Codex CLI's own store, never in this proxy.
type CodexLogin struct {
	mu      sync.Mutex
	rpc     *codexRPC
	closeFn func()
	cancel  context.CancelFunc
	busy    bool
	done    chan struct{} // closed by Close: in-flight Check returns at once
	probe   func() ([]string, error)
	LoginID string `json:"login_id"`
	AuthURL string `json:"auth_url"`
}

func BeginCodexLogin(bin string) (*CodexLogin, error) {
	path, err := resolveCodex(bin)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	deadline := time.AfterFunc(15*time.Second, cancel)
	s, closeFn, err := startCodex(ctx, path)
	if err != nil {
		deadline.Stop()
		cancel()
		return nil, err
	}
	raw, err := s.request("account/login/start", map[string]any{"type": "chatgpt", "useHostedLoginSuccessPage": true, "appBrand": "chatgpt"})
	deadline.Stop()
	if err != nil {
		closeFn()
		cancel()
		return nil, err
	}
	var result struct {
		Type    string `json:"type"`
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		closeFn()
		cancel()
		return nil, err
	}
	u, err := url.Parse(result.AuthURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || result.Type != "chatgpt" || result.LoginID == "" {
		closeFn()
		cancel()
		return nil, errors.New("invalid Codex login response")
	}
	return &CodexLogin{rpc: s, closeFn: closeFn, cancel: cancel, done: make(chan struct{}), probe: s.accountAndModels, LoginID: result.LoginID, AuthURL: result.AuthURL}, nil
}

// codexCheckTimeout bounds one login status probe.
var codexCheckTimeout = 10 * time.Second

// Check probes the login without holding l.mu across the RPC, so Close
// (cancel) never waits on it. One probe runs at a time; a concurrent call
// reports "not complete yet". A probe past the deadline ends the session,
// because the RPC stream can no longer be trusted.
func (l *CodexLogin) Check() ([]string, bool, error) {
	l.mu.Lock()
	if l.closeFn == nil {
		l.mu.Unlock()
		return nil, false, errors.New("login session closed")
	}
	if l.busy {
		l.mu.Unlock()
		return nil, false, nil
	}
	l.busy = true
	probe, done := l.probe, l.done
	l.mu.Unlock()
	type res struct {
		models []string
		err    error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := probe()
		ch <- res{m, err}
	}()
	timer := time.NewTimer(codexCheckTimeout)
	defer timer.Stop()
	var r res
	select {
	case r = <-ch:
	case <-done:
		return nil, false, errors.New("login session closed")
	case <-timer.C:
		l.Close()
		return nil, false, errors.New("codex login check timed out; start the login again")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.busy = false
	if l.closeFn == nil {
		return nil, false, errors.New("login session closed")
	}
	if r.err != nil {
		var known backend.Failure
		if errors.As(r.err, &known) && strings.HasPrefix(string(known), "AUTH_REQUIRED") {
			return nil, false, nil
		}
		return nil, false, r.err
	}
	l.closeLocked()
	return r.models, true, nil
}

// Closed reports whether the session has ended (cancelled or timed out).
func (l *CodexLogin) Closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeFn == nil
}

func (l *CodexLogin) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeLocked()
}

func (l *CodexLogin) closeLocked() {
	if l.closeFn != nil {
		// never wait for the App Server to exit: cancel, then reap async
		l.cancel()
		go l.closeFn()
		l.closeFn = nil
		l.rpc = nil
		if l.done != nil {
			close(l.done)
		}
	}
}

func LogoutCodex(bin string) error {
	path, err := resolveCodex(bin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, closeFn, err := startCodex(ctx, path)
	if err != nil {
		return err
	}
	defer closeFn()
	_, err = s.request("account/logout", map[string]any{})
	return err
}
