package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CodexLogin keeps the official App Server alive for its browser callback.
// Credentials remain in the Codex CLI's own store, never in this proxy.
type CodexLogin struct {
	mu      sync.Mutex
	rpc     *codexRPC
	closeFn func()
	cancel  context.CancelFunc
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
	return &CodexLogin{rpc: s, closeFn: closeFn, cancel: cancel, LoginID: result.LoginID, AuthURL: result.AuthURL}, nil
}

func (l *CodexLogin) Check() ([]string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rpc == nil {
		return nil, false, errors.New("login session closed")
	}
	models, err := l.rpc.accountAndModels()
	if err != nil {
		var known knownFailure
		if errors.As(err, &known) && strings.HasPrefix(string(known), "AUTH_REQUIRED") {
			return nil, false, nil
		}
		return nil, false, err
	}
	l.closeLocked()
	return models, true, nil
}

func (l *CodexLogin) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeLocked()
}

func (l *CodexLogin) closeLocked() {
	if l.closeFn != nil {
		l.closeFn()
		l.cancel()
		l.closeFn = nil
		l.rpc = nil
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
