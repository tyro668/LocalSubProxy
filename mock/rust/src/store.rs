//! In-memory persistence, same contract as the Node MemoryStore (sdk/node/src/store.ts).
//! Callers hold the gateway lock.
use serde_json::Value;
use std::collections::HashMap;

pub const TERMINAL: [&str; 3] = ["SUCCEEDED", "FAILED", "CANCELLED"];

#[derive(Clone, Default)]
pub struct DeviceAuth {
    pub code_hash: String,
    pub user_code: String,
    pub tenant: String,
    pub name: String,
    pub status: String,
    pub device_id: String,
    pub owner: String,
    pub pub_: Vec<u8>,
    pub expires: i64,
    pub last_poll: i64,
    pub interval: i64,
}

#[derive(Clone, Default)]
pub struct Device {
    pub id: String,
    pub tenant: String,
    pub owner: String,
    pub name: String,
    pub status: String,
    pub e2ee_key: String,
    pub pub_: Vec<u8>,
    pub created: i64, // unix seconds
}

#[derive(Clone)]
pub struct Idempotency {
    pub tenant: String,
    pub owner: String,
    pub task_id: String,
    pub body_hash: String,
}

#[derive(Clone, Debug)]
pub struct Event {
    pub seq: i64,
    pub ty: String,
    pub data: Value,
}

pub struct TaskRecord {
    pub id: String,
    pub tenant: String,
    pub owner: String,
    pub device: String,
    pub model: String,
    pub state: String,
    pub events: Vec<Event>,
    /// ms since epoch when the task ended (retention); 0 while running.
    pub finished: i64,
}

pub enum StoreErr {
    Conflict(String),
    NotFound(String),
}

impl StoreErr {
    pub fn msg(&self) -> &str {
        match self {
            StoreErr::Conflict(m) | StoreErr::NotFound(m) => m,
        }
    }
}

#[derive(Default)]
pub struct Store {
    auths: Vec<DeviceAuth>,
    devices: Vec<Device>,
    idem: HashMap<(String, String, String), Idempotency>,
    tasks: HashMap<String, TaskRecord>,
}

impl Store {
    pub fn put_device_auth(&mut self, a: DeviceAuth) {
        if let Some(x) = self.auths.iter_mut().find(|x| x.code_hash == a.code_hash) {
            *x = a;
        } else {
            self.auths.push(a);
        }
    }
    pub fn get_device_auth(&self, h: &str) -> Option<DeviceAuth> {
        self.auths.iter().find(|a| a.code_hash == h).cloned()
    }
    pub fn device_auth_by_user_code(&self, tenant: &str, code: &str) -> Option<DeviceAuth> {
        self.auths.iter().find(|a| a.tenant == tenant && a.user_code == code).cloned()
    }
    pub fn put_device(&mut self, d: Device) {
        if let Some(x) = self.devices.iter_mut().find(|x| x.id == d.id) {
            *x = d;
        } else {
            self.devices.push(d);
        }
    }
    pub fn get_device(&self, id: &str) -> Option<Device> {
        self.devices.iter().find(|d| d.id == id).cloned()
    }
    pub fn list_devices(&self, tenant: &str, owner: Option<&str>) -> Vec<Device> {
        self.devices.iter().filter(|d| d.tenant == tenant && owner.map(|o| d.owner == o).unwrap_or(true)).cloned().collect()
    }
    pub fn get_idempotency(&self, t: &str, o: &str, k: &str) -> Option<Idempotency> {
        self.idem.get(&(t.into(), o.into(), k.into())).cloned()
    }
    pub fn create_task(&mut self, t: TaskRecord, idem_key: Option<&str>, rec: Option<Idempotency>) -> Result<(), StoreErr> {
        if self.tasks.contains_key(&t.id) {
            return Err(StoreErr::Conflict(format!("task {} exists", t.id)));
        }
        if let (Some(k), Some(rec)) = (idem_key, rec) {
            let key = (rec.tenant.clone(), rec.owner.clone(), k.to_string());
            if self.idem.contains_key(&key) {
                return Err(StoreErr::Conflict("idempotency key taken".into()));
            }
            self.idem.insert(key, rec);
        }
        self.tasks.insert(t.id.clone(), t);
        Ok(())
    }
    pub fn get_task(&self, id: &str) -> Option<&TaskRecord> {
        self.tasks.get(id)
    }
    pub fn set_state(&mut self, id: &str, state: &str) {
        if let Some(t) = self.tasks.get_mut(id) {
            t.state = state.into();
        }
    }
    pub fn append_event(&mut self, id: &str, ev: Event) -> Result<(), StoreErr> {
        let Some(t) = self.tasks.get_mut(id) else { return Ok(()) };
        if t.events.iter().any(|e| e.seq == ev.seq) {
            return Err(StoreErr::Conflict(format!("task {}: seq {} already stored", id, ev.seq)));
        }
        t.events.push(ev);
        Ok(())
    }
    pub fn finish_task(&mut self, id: &str, ev: Event, state: &str) -> Result<(), StoreErr> {
        let Some(t) = self.tasks.get_mut(id) else { return Err(StoreErr::NotFound(format!("task {} not found", id))) };
        if TERMINAL.contains(&t.state.as_str()) {
            return Err(StoreErr::Conflict(format!("task {} already {}", id, t.state)));
        }
        if t.events.iter().any(|e| e.seq == ev.seq) {
            return Err(StoreErr::Conflict(format!("task {}: seq {} already stored", id, ev.seq)));
        }
        t.events.push(ev);
        t.state = state.into();
        t.finished = crate::lmbp::now_ms();
        Ok(())
    }
    /// Retention (spec §14.4, errata E6): finished tasks older than cutoff (ms) go
    /// with their idempotency records; expired pairings too. Returns the tasks removed.
    pub fn purge(&mut self, cutoff: i64) -> usize {
        let before = self.tasks.len();
        self.tasks.retain(|_, t| !(TERMINAL.contains(&t.state.as_str()) && t.finished > 0 && t.finished < cutoff));
        let tasks = &self.tasks;
        self.idem.retain(|_, r| tasks.contains_key(&r.task_id));
        self.auths.retain(|a| !(a.expires > 0 && a.expires < cutoff));
        before - self.tasks.len()
    }
}
