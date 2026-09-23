package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const Version = "connector.v1"

// Frame is the bounded control envelope used on the connector WebSocket.
// ReplyTo correlates a durable control response with its request.
type Frame struct {
	Protocol     string          `json:"protocol"`
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	ReplyTo      string          `json:"reply_to,omitempty"`
	SessionEpoch int64           `json:"session_epoch,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type ResumeCursor struct {
	TaskID      string `json:"task_id"`
	AttemptID   string `json:"attempt_id"`
	State       string `json:"state"`
	AckSeq      int64  `json:"ack_seq"`
	StartIntent bool   `json:"start_intent"`
}

type Hello struct {
	Heartbeat Heartbeat      `json:"heartbeat"`
	Journal   []ResumeCursor `json:"journal,omitempty"`
}

type Welcome struct {
	HeartbeatSeconds int              `json:"heartbeat_seconds"`
	MaxMessageBytes  int              `json:"max_message_bytes"`
	ServerAckSeq     map[string]int64 `json:"server_ack_seq,omitempty"`
	Attempts         []AttemptState   `json:"attempts,omitempty"`
}

type AttemptState struct {
	TaskID     string    `json:"task_id"`
	AttemptID  string    `json:"attempt_id"`
	State      string    `json:"state"`
	AckSeq     int64     `json:"ack_seq"`
	LeaseUntil time.Time `json:"lease_until,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Input struct {
	Messages     []Message `json:"messages"`
	ArtifactRefs []string  `json:"artifact_refs,omitempty"`
}

type Target struct {
	DeviceID  string `json:"device_id"`
	AdapterID string `json:"adapter_id"`
	ModelID   string `json:"model_id"`
}

type CreateTask struct {
	Target    Target `json:"target"`
	Operation string `json:"operation"`
	Input     Input  `json:"input"`
	Options   struct {
		QueueTTLSeconds   int `json:"queue_ttl_seconds"`
		MaxRuntimeSeconds int `json:"max_runtime_seconds"`
	} `json:"options"`
}

func (c CreateTask) Validate() error {
	if (c.Operation != "chat.generate" && c.Operation != "agent.run") || c.Target.DeviceID == "" || c.Target.AdapterID == "" || c.Target.ModelID == "" {
		return errors.New("unsupported operation or missing target")
	}
	if len(c.Input.Messages) == 0 || len(c.Input.Messages) > 100 {
		return errors.New("messages must contain 1 to 100 items")
	}
	for _, m := range c.Input.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return errors.New("unsupported message role")
		}
		if len(m.Content) > 1<<20 {
			return errors.New("message too large")
		}
	}
	if len(c.Input.ArtifactRefs) > 5 {
		return errors.New("at most five artifact references are allowed")
	}
	seen := map[string]bool{}
	for _, id := range c.Input.ArtifactRefs {
		if len(id) != len("artifact_")+32 || id[:len("artifact_")] != "artifact_" || seen[id] {
			return errors.New("invalid artifact reference")
		}
		if _, err := hex.DecodeString(id[len("artifact_"):]); err != nil {
			return errors.New("invalid artifact reference")
		}
		seen[id] = true
	}
	b, _ := json.Marshal(c)
	if len(b) > 128<<10 {
		return errors.New("inline task input exceeds 128 KiB; use artifact references")
	}
	return nil
}

func Digest(c CreateTask) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Event struct {
	Seq  int64           `json:"seq"`
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

func NewEvent(seq int64, kind string, data any) Event {
	b, _ := json.Marshal(data)
	return Event{Seq: seq, Kind: kind, At: time.Now().UTC(), Data: b}
}

type Task struct {
	ID         string     `json:"task_id"`
	AttemptID  string     `json:"attempt_id,omitempty"`
	FenceToken int64      `json:"fence_token,omitempty"`
	State      string     `json:"state"`
	Request    CreateTask `json:"request"`
	Digest     string     `json:"digest"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LeaseUntil time.Time  `json:"lease_until,omitempty"`
	AckSeq     int64      `json:"ack_seq"`
	Events     []Event    `json:"events,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type Command struct {
	Protocol   string     `json:"protocol"`
	Type       string     `json:"type"`
	DeviceID   string     `json:"device_id"`
	TaskID     string     `json:"task_id"`
	AttemptID  string     `json:"attempt_id"`
	FenceToken int64      `json:"fence_token"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Request    CreateTask `json:"request,omitempty"`
	Digest     string     `json:"payload_hash,omitempty"`
	Signature  string     `json:"authorization,omitempty"`
}

type EventBatch struct {
	TaskID     string  `json:"task_id"`
	AttemptID  string  `json:"attempt_id"`
	FenceToken int64   `json:"fence_token"`
	Events     []Event `json:"events"`
}

type AdapterHealth struct {
	AdapterID         string   `json:"adapter_id"`
	AdapterType       string   `json:"adapter_type"`
	RuntimeVersion    string   `json:"runtime_version,omitempty"`
	Models            []string `json:"models"`
	Operations        []string `json:"operations"`
	Health            string   `json:"health"`
	AuthState         string   `json:"auth_state"`
	AccountScope      string   `json:"account_scope"`
	InferenceLocation string   `json:"inference_location"`
	FreeSlots         int      `json:"free_slots"`
}

type Heartbeat struct {
	Protocol string          `json:"protocol"`
	DeviceID string          `json:"device_id"`
	Adapter  AdapterHealth   `json:"adapter"`
	Adapters []AdapterHealth `json:"adapters,omitempty"`
}
