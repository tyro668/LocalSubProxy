package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

func (s *Server) pairingDigest(code string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.SAASToken))
	_, _ = mac.Write([]byte(s.cfg.DeviceID + ":" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// pairing uses the provisioned device credential before the device is bound.
func (s *Server) pairing(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || !secureEqual(strings.TrimPrefix(auth, "Bearer "), s.cfg.DeviceToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		http.Error(w, "random source unavailable", 503)
		return
	}
	code := formatPairingCode(n.Int64())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Revoked {
		http.Error(w, "device revoked", 403)
		return
	}
	if s.state.OwnerID != "" {
		http.Error(w, "device already bound", 409)
		return
	}
	oldHash, oldExpiry, oldAttempts := s.state.PairingHash, s.state.PairingExpires, s.state.PairingAttempts
	expires := time.Now().UTC().Add(5 * time.Minute)
	s.state.PairingHash, s.state.PairingExpires, s.state.PairingAttempts = s.pairingDigest(code), expires, 0
	if err := s.persist(); err != nil {
		s.state.PairingHash, s.state.PairingExpires, s.state.PairingAttempts = oldHash, oldExpiry, oldAttempts
		http.Error(w, "storage error", 503)
		return
	}
	reply(w, 200, map[string]any{"device_id": s.cfg.DeviceID, "short_code": code, "expires_at": expires})
}

func formatPairingCode(n int64) string {
	return fmt.Sprintf("%08d", n)
}

func validOwnerID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) bind(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	var req struct {
		UserID    string `json:"user_id"`
		ShortCode string `json:"short_code"`
	}
	if !decode(w, r, &req, 4096) {
		return
	}
	if !validOwnerID(req.UserID) || len(req.ShortCode) != 8 {
		http.Error(w, "invalid binding request", 400)
		return
	}
	for _, c := range req.ShortCode {
		if c < '0' || c > '9' {
			http.Error(w, "invalid binding request", 400)
			return
		}
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Revoked {
		http.Error(w, "device revoked", 403)
		return
	}
	if s.state.OwnerID != "" {
		http.Error(w, "device already bound", 409)
		return
	}
	if s.state.PairingHash == "" || !time.Now().Before(s.state.PairingExpires) || s.state.PairingAttempts >= 5 {
		http.Error(w, "pairing code expired or unavailable", 409)
		return
	}
	if !secureEqual(s.state.PairingHash, s.pairingDigest(req.ShortCode)) {
		old := s.state.PairingAttempts
		s.state.PairingAttempts++
		if err := s.persist(); err != nil {
			s.state.PairingAttempts = old
			http.Error(w, "storage error", 503)
			return
		}
		http.Error(w, "invalid pairing code", 409)
		return
	}
	old := s.state
	s.state.OwnerID, s.state.PairingHash, s.state.PairingExpires, s.state.PairingAttempts = req.UserID, "", time.Time{}, 0
	if err := s.persist(); err != nil {
		s.state = old
		http.Error(w, "storage error", 503)
		return
	}
	s.notify()
	reply(w, 200, map[string]string{"device_id": s.cfg.DeviceID, "owner_id": req.UserID, "status": "bound"})
}
