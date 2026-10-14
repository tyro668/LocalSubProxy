// Package store is the persistence boundary of the lmbp reference gateway. The
// reference implementation is SQLite (pure Go); a Postgres implementation can
// satisfy the same Store interface. Prompt/output columns can be sealed with
// AES-256-GCM at rest via Cipher.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

type Tenant struct {
	ID         string
	Name       string
	APIKeyHash string // hex SHA-256 of the tenant API key; the key itself is never stored
	Profile    TenantProfile
}

// TenantProfile is the SaaS identity published (signed) in the gateway
// metadata and shown to the user on the desktop consent screen.
type TenantProfile struct {
	DisplayName     string   `json:"display_name,omitempty"`
	LogoURI         string   `json:"logo_uri,omitempty"`
	HomepageURI     string   `json:"homepage_uri,omitempty"`
	PolicyURI       string   `json:"policy_uri,omitempty"`
	TosURI          string   `json:"tos_uri,omitempty"`
	PairingURI      string   `json:"pairing_uri,omitempty"`
	RequestedModels []string `json:"requested_models,omitempty"`
	RequestedScopes []string `json:"requested_scopes,omitempty"`
}

// DeviceAuth is one pending RFC 8628 device authorization.
type DeviceAuth struct {
	DeviceCodeHash string
	UserCode       string
	TenantID       string
	PublicKey      []byte
	DeviceName     string
	Status         string // pending | approved | denied | consumed
	OwnerID        string
	DeviceID       string
	ExpiresAt      time.Time
	Interval       int
	LastPoll       time.Time
	Attempts       int
}

type Device struct {
	ID        string
	TenantID  string
	OwnerID   string
	Name      string
	PublicKey []byte
	Status    string // active | revoked
	CreatedAt time.Time
	E2EEKey   string // device-signed X25519 key statement (JWS); empty if none
}

// Idempotency maps (tenant, end user, Idempotency-Key) to the task it created.
type Idempotency struct {
	TenantID  string
	OwnerID   string
	Key       string
	TaskID    string
	BodyHash  string // hex SHA-256 of the request body
	CreatedAt time.Time
}

// ErrExists is returned when an idempotency key is already recorded.
var ErrExists = errors.New("already exists")

type Task struct {
	ID        string
	TenantID  string
	OwnerID   string
	DeviceID  string
	Model     string
	State     string // RUNNING | SUCCEEDED | FAILED | CANCELLED
	Request   []byte // JSON; sealed when a Cipher is configured
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Event struct {
	TaskID string
	Seq    int64
	Kind   string
	Data   []byte // sealed when a Cipher is configured
	At     time.Time
}

type Store interface {
	PutTenant(ctx context.Context, t Tenant) error
	TenantByAPIKeyHash(ctx context.Context, hash string) (Tenant, error)
	GetTenant(ctx context.Context, id string) (Tenant, error)

	CreateDeviceAuth(ctx context.Context, d DeviceAuth) error
	DeviceAuthByCodeHash(ctx context.Context, hash string) (DeviceAuth, error)
	DeviceAuthByUserCode(ctx context.Context, tenantID, userCode string) (DeviceAuth, error)
	UpdateDeviceAuth(ctx context.Context, d DeviceAuth) error

	CreateDevice(ctx context.Context, d Device) error
	GetDevice(ctx context.Context, id string) (Device, error)
	ListDevices(ctx context.Context, tenantID, ownerID string) ([]Device, error)
	SetDeviceStatus(ctx context.Context, id, status string) error
	SetDeviceE2EEKey(ctx context.Context, id, stmt string) error

	GetIdempotency(ctx context.Context, tenantID, ownerID, key string) (Idempotency, error)

	// CreateTask stores a new task and, when idem is non-nil, its
	// Idempotency-Key record, atomically: both are committed or neither is,
	// so a key never exists without its task. ErrExists if the key
	// (tenant, owner, key) is already taken; then nothing is stored.
	CreateTask(ctx context.Context, t Task, idem *Idempotency) error
	// SetTaskState sets a non-terminal state (QUEUED, RUNNING). It never
	// overwrites a terminal state; terminal states are only written by
	// FinishTask.
	SetTaskState(ctx context.Context, id, state string) error
	GetTask(ctx context.Context, id string) (Task, error)
	// AppendEvent stores a non-terminal event; ErrExists if (task, seq) is
	// already stored (a seq is never reused).
	AppendEvent(ctx context.Context, e Event) error
	// FinishTask atomically stores the terminal event e (task.done /
	// task.error) AND sets the terminal state (SUCCEEDED, FAILED,
	// CANCELLED): both are committed or neither is. ErrExists if e.Seq is
	// taken or the task already has a terminal state; ErrNotFound if the task
	// does not exist. Recovery relies on this: a stored terminal event always
	// agrees with the stored state.
	FinishTask(ctx context.Context, e Event, state string) error
	ListEvents(ctx context.Context, taskID string) ([]Event, error)

	// Purge deletes finished tasks (and their events) and expired device
	// authorizations older than the cutoff. Returns removed task count.
	Purge(ctx context.Context, cutoff time.Time) (int, error)
	Close() error
}

// SecretStore is optionally implemented by a Store to keep small secrets
// such as the gateway signing seed (SQLite and Memory implement it).
type SecretStore interface {
	GetSecret(ctx context.Context, name string) ([]byte, error) // ErrNotFound if absent
	PutSecret(ctx context.Context, name string, v []byte) error
}

// Cipher seals prompt/output payloads. A nil *Cipher stores plaintext.
type Cipher struct{ aead cipher.AEAD }

func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Cipher{g}, nil
}

var sealedPrefix = []byte("enc1:")

func (c *Cipher) Seal(p []byte) []byte {
	if c == nil {
		return p
	}
	nonce := make([]byte, c.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := append([]byte{}, sealedPrefix...)
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, p, nil)
}

func (c *Cipher) Open(b []byte) ([]byte, error) {
	if len(b) < len(sealedPrefix) || string(b[:len(sealedPrefix)]) != string(sealedPrefix) {
		return b, nil // plaintext row (written before encryption was enabled)
	}
	if c == nil {
		return nil, errors.New("encrypted row but no key configured")
	}
	b = b[len(sealedPrefix):]
	n := c.aead.NonceSize()
	if len(b) < n {
		return nil, errors.New("short ciphertext")
	}
	return c.aead.Open(nil, b[:n], b[n:], nil)
}
