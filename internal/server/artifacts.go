package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"localsubproxy/internal/durable"
)

const maxTextArtifact = 1 << 20

type artifactRecord struct {
	TaskID    string    `json:"task_id,omitempty"`
	SHA256    string    `json:"sha256"`
	Bytes     int       `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) artifactPath(id string) string {
	return filepath.Join(filepath.Dir(s.cfg.DataFile), "artifacts", id+".txt")
}

func validArtifactID(id string) bool {
	if len(id) != len("artifact_")+32 || !strings.HasPrefix(id, "artifact_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "artifact_"))
	return err == nil
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) || !method(w, r, http.MethodPost) {
		return
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "text/plain" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		http.Error(w, "only UTF-8 text/plain artifacts are supported", 415)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxTextArtifact+1))
	if err != nil {
		http.Error(w, "invalid artifact body", 400)
		return
	}
	if len(b) == 0 || len(b) > maxTextArtifact || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		http.Error(w, "text artifact must be nonempty UTF-8 and at most 1 MiB", 413)
		return
	}
	hash := sha256.Sum256(b)
	id := randomID("artifact_")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Revoked {
		http.Error(w, "device revoked", 403)
		return
	}
	if s.cfg.RequireBinding && s.state.OwnerID == "" {
		http.Error(w, "device not bound", 403)
		return
	}
	var pendingBytes int64
	for _, meta := range s.state.Artifacts {
		if meta != nil && meta.TaskID == "" {
			pendingBytes += int64(meta.Bytes)
		}
	}
	if pendingBytes+int64(len(b)) > 64<<20 {
		http.Error(w, "unattached artifact capacity reached", 507)
		return
	}
	path := s.artifactPath(id)
	if err := durable.Write(path, b); err != nil {
		http.Error(w, "storage error", 503)
		return
	}
	s.state.Artifacts[id] = &artifactRecord{SHA256: hex.EncodeToString(hash[:]), Bytes: len(b), CreatedAt: time.Now().UTC()}
	if err := s.persist(); err != nil {
		delete(s.state.Artifacts, id)
		_ = os.Remove(path)
		http.Error(w, "storage error", 503)
		return
	}
	reply(w, 201, map[string]any{"artifact_id": id, "sha256": s.state.Artifacts[id].SHA256, "bytes": len(b)})
}

func (s *Server) deviceArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, http.MethodGet) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/connector/artifacts/")
	if !validArtifactID(id) {
		http.NotFound(w, r)
		return
	}
	taskID, attemptID := r.URL.Query().Get("task_id"), r.URL.Query().Get("attempt_id")
	s.mu.Lock()
	meta := s.state.Artifacts[id]
	rec := s.state.Tasks[taskID]
	allowed := meta != nil && rec != nil && meta.TaskID == taskID && rec.Task.AttemptID == attemptID && (rec.Task.State == "ACCEPTED" || rec.Task.State == "RUNNING")
	s.mu.Unlock()
	if !allowed {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(s.artifactPath(id))
	if err != nil || len(b) != meta.Bytes || len(b) > maxTextArtifact {
		http.Error(w, "artifact storage error", 503)
		return
	}
	hash := sha256.Sum256(b)
	if !secureEqual(hex.EncodeToString(hash[:]), meta.SHA256) {
		http.Error(w, "artifact integrity error", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("X-Content-SHA256", meta.SHA256)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// On restart, remove unused uploads after a day and completed task attachments after seven days.
func (s *Server) pruneArtifacts() error {
	now := time.Now().UTC()
	removed := map[string]*artifactRecord{}
	for id, meta := range s.state.Artifacts {
		if meta == nil {
			delete(s.state.Artifacts, id)
			continue
		}
		task := s.state.Tasks[meta.TaskID]
		expired := meta.TaskID == "" && now.Sub(meta.CreatedAt) > 24*time.Hour ||
			meta.TaskID != "" && (task == nil || terminal(task.Task.State) && now.Sub(task.Task.UpdatedAt) > 7*24*time.Hour)
		if expired {
			removed[id] = meta
			delete(s.state.Artifacts, id)
		}
	}
	if len(removed) > 0 {
		if err := s.persist(); err != nil {
			for id, meta := range removed {
				s.state.Artifacts[id] = meta
			}
			return fmt.Errorf("prune artifacts: %w", err)
		}
		for id := range removed {
			_ = os.Remove(s.artifactPath(id))
		}
	}
	dir := filepath.Join(filepath.Dir(s.cfg.DataFile), "artifacts")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".txt")
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".txt") && validArtifactID(id) && s.state.Artifacts[id] == nil {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	return nil
}
