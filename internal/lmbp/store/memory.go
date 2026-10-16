package store

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Memory is an in-memory Store (state is lost on restart). It also
// implements SecretStore.
type Memory struct {
	mu      sync.Mutex
	tenants map[string]Tenant
	auths   map[string]DeviceAuth
	devices map[string]Device
	order   []string
	idem    map[[3]string]Idempotency
	tasks   map[string]Task
	events  map[string][]Event
	secrets map[string][]byte
}

func NewMemory() *Memory {
	return &Memory{tenants: map[string]Tenant{}, auths: map[string]DeviceAuth{}, devices: map[string]Device{},
		idem: map[[3]string]Idempotency{}, tasks: map[string]Task{}, events: map[string][]Event{}, secrets: map[string][]byte{}}
}

func (m *Memory) lock() func() { m.mu.Lock(); return m.mu.Unlock }

func (m *Memory) PutTenant(_ context.Context, t Tenant) error {
	defer m.lock()()
	m.tenants[t.ID] = t
	return nil
}

func (m *Memory) TenantByAPIKeyHash(_ context.Context, h string) (Tenant, error) {
	defer m.lock()()
	for _, t := range m.tenants {
		if t.APIKeyHash == h {
			return t, nil
		}
	}
	return Tenant{}, ErrNotFound
}

func (m *Memory) GetTenant(_ context.Context, id string) (Tenant, error) {
	defer m.lock()()
	t, ok := m.tenants[id]
	if !ok {
		return t, ErrNotFound
	}
	return t, nil
}

func (m *Memory) CreateDeviceAuth(_ context.Context, d DeviceAuth) error {
	defer m.lock()()
	if _, ok := m.auths[d.DeviceCodeHash]; ok {
		return ErrExists
	}
	m.auths[d.DeviceCodeHash] = d
	return nil
}

func (m *Memory) DeviceAuthByCodeHash(_ context.Context, h string) (DeviceAuth, error) {
	defer m.lock()()
	d, ok := m.auths[h]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}

func (m *Memory) DeviceAuthByUserCode(_ context.Context, tenantID, code string) (DeviceAuth, error) {
	defer m.lock()()
	var best DeviceAuth
	found := false
	for _, d := range m.auths {
		if d.TenantID == tenantID && d.UserCode == code && (!found || d.ExpiresAt.After(best.ExpiresAt)) {
			best, found = d, true
		}
	}
	if !found {
		return best, ErrNotFound
	}
	return best, nil
}

func (m *Memory) UpdateDeviceAuth(_ context.Context, d DeviceAuth) error {
	defer m.lock()()
	if _, ok := m.auths[d.DeviceCodeHash]; !ok {
		return ErrNotFound
	}
	m.auths[d.DeviceCodeHash] = d
	return nil
}

func (m *Memory) CreateDevice(_ context.Context, d Device) error {
	defer m.lock()()
	if _, ok := m.devices[d.ID]; ok {
		return ErrExists
	}
	m.devices[d.ID] = d
	m.order = append(m.order, d.ID)
	return nil
}

func (m *Memory) GetDevice(_ context.Context, id string) (Device, error) {
	defer m.lock()()
	d, ok := m.devices[id]
	if !ok {
		return d, ErrNotFound
	}
	d.PublicKey = slices.Clone(d.PublicKey)
	return d, nil
}

func (m *Memory) ListDevices(_ context.Context, tenantID, ownerID string) ([]Device, error) {
	defer m.lock()()
	var out []Device
	for _, id := range m.order {
		if d := m.devices[id]; d.TenantID == tenantID && d.OwnerID == ownerID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *Memory) SetDeviceStatus(_ context.Context, id, status string) error {
	defer m.lock()()
	d, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	d.Status = status
	m.devices[id] = d
	return nil
}

func (m *Memory) SetDeviceE2EEKey(_ context.Context, id, stmt string) error {
	defer m.lock()()
	d, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	d.E2EEKey = stmt
	m.devices[id] = d
	return nil
}

func (m *Memory) GetIdempotency(_ context.Context, tenantID, ownerID, key string) (Idempotency, error) {
	defer m.lock()()
	k := [3]string{tenantID, ownerID, key}
	i, ok := m.idem[k]
	if !ok {
		return i, ErrNotFound
	}
	if _, live := m.tasks[i.TaskID]; !live {
		// Never "key exists, task does not": a dangling record reads as absent.
		delete(m.idem, k)
		return Idempotency{}, ErrNotFound
	}
	return i, nil
}

func (m *Memory) CreateTask(_ context.Context, t Task, i *Idempotency) error {
	defer m.lock()()
	if _, ok := m.tasks[t.ID]; ok {
		return ErrExists
	}
	if i != nil {
		k := [3]string{i.TenantID, i.OwnerID, i.Key}
		if old, ok := m.idem[k]; ok {
			if _, live := m.tasks[old.TaskID]; live {
				return ErrExists
			}
			// dangling (its task is gone): the key is free again
		}
		m.idem[k] = *i
	}
	m.tasks[t.ID] = t
	return nil
}

func (m *Memory) SetTaskState(_ context.Context, id, state string) error {
	defer m.lock()()
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if t.State != "QUEUED" && t.State != "RUNNING" && t.State != "" {
		return nil // never overwrite a terminal state (see Store)
	}
	t.State, t.UpdatedAt = state, time.Now()
	m.tasks[id] = t
	return nil
}

// FinishTask stores the terminal event and state under one lock: both or
// neither (see Store).
func (m *Memory) FinishTask(_ context.Context, e Event, state string) error {
	defer m.lock()()
	t, ok := m.tasks[e.TaskID]
	if !ok {
		return ErrNotFound
	}
	if t.State != "QUEUED" && t.State != "RUNNING" && t.State != "" {
		return ErrExists
	}
	for _, x := range m.events[e.TaskID] {
		if x.Seq == e.Seq {
			return ErrExists
		}
	}
	m.events[e.TaskID] = append(m.events[e.TaskID], e)
	t.State, t.UpdatedAt = state, time.Now()
	m.tasks[e.TaskID] = t
	return nil
}

func (m *Memory) GetTask(_ context.Context, id string) (Task, error) {
	defer m.lock()()
	t, ok := m.tasks[id]
	if !ok {
		return t, ErrNotFound
	}
	return t, nil
}

func (m *Memory) AppendEvent(_ context.Context, e Event) error {
	defer m.lock()()
	for _, x := range m.events[e.TaskID] {
		if x.Seq == e.Seq {
			return ErrExists // a seq is never reused (same as the SQLite primary key)
		}
	}
	m.events[e.TaskID] = append(m.events[e.TaskID], e)
	return nil
}

func (m *Memory) ListEvents(_ context.Context, taskID string) ([]Event, error) {
	defer m.lock()()
	ev := slices.Clone(m.events[taskID])
	slices.SortFunc(ev, func(a, b Event) int { return int(a.Seq - b.Seq) })
	return ev, nil
}

func (m *Memory) Purge(_ context.Context, cutoff time.Time) (int, error) {
	defer m.lock()()
	n := 0
	for id, t := range m.tasks {
		if t.State != "RUNNING" && t.State != "QUEUED" && t.UpdatedAt.Before(cutoff) {
			delete(m.tasks, id)
			delete(m.events, id)
			n++
		}
	}
	// Idempotency records go with their task: never a key without its task.
	for k, i := range m.idem {
		if _, live := m.tasks[i.TaskID]; !live {
			delete(m.idem, k)
		}
	}
	for h, d := range m.auths {
		if d.ExpiresAt.Before(cutoff) {
			delete(m.auths, h)
		}
	}
	return n, nil
}

func (m *Memory) Close() error { return nil }

func (m *Memory) GetSecret(_ context.Context, name string) ([]byte, error) {
	defer m.lock()()
	v, ok := m.secrets[name]
	if !ok {
		return nil, ErrNotFound
	}
	return slices.Clone(v), nil
}

func (m *Memory) PutSecret(_ context.Context, name string, v []byte) error {
	defer m.lock()()
	m.secrets[name] = slices.Clone(v)
	return nil
}
