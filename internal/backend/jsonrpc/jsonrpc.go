// Package jsonrpc frames the newline-delimited JSON-RPC used by local CLIs
// (Codex app-server, ACP). It never answers a peer request with anything but
// an error: local CLIs are not granted tools, approvals or credentials.
package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// MaxMessage bounds one encoded message.
const MaxMessage = 1 << 20

// MethodNotFound is the JSON-RPC code for refused peer requests.
const MethodNotFound = -32601

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is any JSON-RPC frame. Version is omitted when empty (Codex's
// app-server dialect); ACP peers set "2.0".
type Message struct {
	Version string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsRequest: the peer asks us to do something and waits for an answer.
func (m Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification: a method call without an ID.
func (m Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse: an answer to one of our requests.
func (m Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// IntID returns a numeric ID.
func (m Message) IntID() (int, bool) {
	var id int
	if len(m.ID) == 0 || json.Unmarshal(m.ID, &id) != nil {
		return 0, false
	}
	return id, true
}

// Parse decodes one line.
func Parse(line []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	if m.Method == "" && len(m.ID) == 0 {
		return m, errors.New("invalid JSON-RPC message: neither a call nor a response")
	}
	return m, nil
}

// Encode returns m as one newline-terminated line.
func Encode(m Message) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxMessage {
		return nil, errors.New("JSON-RPC message too large")
	}
	return append(b, '\n'), nil
}

// Request encodes a call with a numeric ID.
func Request(version string, id int, method string, params any) ([]byte, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return Encode(Message{Version: version, ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: raw})
}

// Notify encodes a call without an ID.
func Notify(version, method string, params any) ([]byte, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return Encode(Message{Version: version, Method: method, Params: raw})
}

// Refuse answers a peer request with MethodNotFound.
func Refuse(version string, id json.RawMessage, message string) ([]byte, error) {
	return Encode(Message{Version: version, ID: id, Error: &Error{Code: MethodNotFound, Message: message}})
}
