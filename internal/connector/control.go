package connector

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"localsubproxy/internal/durable"
)

type adapterView struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url,omitempty"`
	CodexBin  string   `json:"codex_bin,omitempty"`
	Models    []string `json:"models"`
	Enabled   bool     `json:"enabled"`
	HasAPIKey bool     `json:"has_api_key"`
}

func viewAdapter(a AdapterConfig) adapterView {
	return adapterView{ID: a.ID, Type: a.Type, Name: a.Name, BaseURL: a.BaseURL, CodexBin: a.CodexBin, Models: a.Models, Enabled: a.Enabled, HasAPIKey: a.APIKey != ""}
}

func (x *Connector) saveAdaptersLocked() error {
	if x.cfg.AdapterFile == "" {
		return errors.New("adapter file not configured")
	}
	list := make([]AdapterConfig, 0, len(x.adapters))
	for _, a := range x.adapters {
		list = append(list, a)
	}
	b, err := x.cipher.Seal(list)
	if err != nil {
		return err
	}
	return durable.Write(x.cfg.AdapterFile, b)
}

func controlToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("control token file required")
	}
	if b, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(b))
		if len(token) != 64 {
			return "", errors.New("invalid control token file")
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := durable.Write(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

func (x *Connector) startControl(ctx context.Context) error {
	if x.cfg.ControlAddr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(x.cfg.ControlAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("control listener requires a loopback IP address")
	}
	token, err := controlToken(x.cfg.ControlTokenFile)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", x.cfg.ControlAddr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		x.mu.Lock()
		running := len(x.running)
		journalBytes := x.journalBytes
		x.mu.Unlock()
		writeJSON(w, 200, map[string]any{"device_id": x.cfg.DeviceID, "server_url": x.cfg.ServerURL, "running_tasks": running, "journal_bytes": journalBytes})
	})
	mux.HandleFunc("/v1/adapters", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			x.mu.Lock()
			list := make([]adapterView, 0, len(x.adapters))
			for _, a := range x.adapters {
				list = append(list, viewAdapter(a))
			}
			x.mu.Unlock()
			writeJSON(w, 200, map[string]any{"adapters": list})
		case http.MethodPost:
			a, ok := readAdapter(w, r)
			if !ok {
				return
			}
			if err := validateAdapter(&a); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			x.mu.Lock()
			if _, exists := x.adapters[a.ID]; exists {
				x.mu.Unlock()
				http.Error(w, "adapter already exists", 409)
				return
			}
			x.adapters[a.ID] = a
			err := x.saveAdaptersLocked()
			if err != nil {
				delete(x.adapters, a.ID)
			}
			x.mu.Unlock()
			if err != nil {
				http.Error(w, "storage error", 503)
				return
			}
			writeJSON(w, 201, viewAdapter(a))
			go x.refreshHeartbeat()
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
	mux.HandleFunc("/v1/adapters/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/adapters/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			a, ok := readAdapter(w, r)
			if !ok {
				return
			}
			if a.ID != id {
				http.Error(w, "adapter ID mismatch", 400)
				return
			}
			x.mu.Lock()
			old, exists := x.adapters[id]
			if !exists {
				x.mu.Unlock()
				http.NotFound(w, r)
				return
			}
			if a.APIKey == "" {
				a.APIKey = old.APIKey
			}
			x.mu.Unlock()
			if err := validateAdapter(&a); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			x.mu.Lock()
			x.adapters[id] = a
			err := x.saveAdaptersLocked()
			if err != nil {
				x.adapters[id] = old
			}
			x.mu.Unlock()
			if err != nil {
				http.Error(w, "storage error", 503)
				return
			}
			writeJSON(w, 200, viewAdapter(a))
			go x.refreshHeartbeat()
		case http.MethodDelete:
			x.mu.Lock()
			old, exists := x.adapters[id]
			if !exists {
				x.mu.Unlock()
				http.NotFound(w, r)
				return
			}
			for _, t := range x.j.Tasks {
				if t.Command.Request.Target.AdapterID == id && !isTerminal(t.State) {
					x.mu.Unlock()
					http.Error(w, "adapter has an active task", 409)
					return
				}
			}
			delete(x.adapters, id)
			err := x.saveAdaptersLocked()
			if err != nil {
				x.adapters[id] = old
			}
			x.mu.Unlock()
			if err != nil {
				http.Error(w, "storage error", 503)
				return
			}
			w.WriteHeader(204)
			go x.refreshHeartbeat()
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "origin forbidden", 403)
			return
		}
		if r.Host != x.cfg.ControlAddr {
			http.Error(w, "invalid host", 403)
			return
		}
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(auth) != len(token) || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("control listener: %v\n", err)
		}
	}()
	return nil
}

func (x *Connector) refreshHeartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_ = x.heartbeat(ctx)
}

func readAdapter(w http.ResponseWriter, r *http.Request) (AdapterConfig, bool) {
	var a AdapterConfig
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil {
		http.Error(w, "invalid adapter JSON", 400)
		return a, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "trailing adapter JSON", 400)
		return a, false
	}
	return a, true
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
