# Desktop kernel ↔ UI contract (multi-SaaS)

The Flutter app talks to the Go kernel (`cmd/localsubproxy_native`) through
`LSP_Start(json)` / `LSP_Call(json)` / `LSP_Free(ptr)`. Every reply is
`{"ok":true,"data":…}` or `{"ok":false,"error":"…"}`. The Dart side parses
status with `desktop/lib/status_contract.dart` (test:
`desktop/test/status_contract_test.dart`).

## Connections

One desktop can be paired with several SaaS gateways at the same time. Each
pairing is a **binding** (`connections[].id`, a local id such as
`b_7f3a91`). Each binding has its own device key, its own lmbp WebSocket
session, its own model allow-list and its own concurrency cap. Device ids are
only unique per gateway, so always address a connection by `id`.

## `status`

```json
{
  "protocol": "lmbp",
  "device_name": "roman-mbp",
  "audit": true,
  "paired": true,
  "running_tasks": 3,
  "max_concurrency": 4,
  "connections_total": 2,
  "connections_online": 1,
  "pairing_state": "idle",
  "pairing_error": "",
  "pair_id": "",
  "connections": [
    {
      "id": "b_7f3a91", "saas_name": "Acme Chat", "issuer": "https://gw.acme.example", "tenant": "acme",
      "device_id": "dev_01HZX8", "display_url": "https://chat.acme.example", "remote_url": "", "server_url": "https://gw.acme.example",
      "state": "online", "connected": true, "remote_connected": true, "gateway_connected": true,
      "transport": "wss", "e2ee": true, "running_tasks": 2, "max_concurrency": 2,
      "allow": ["ollama/*", "openai/gpt-4o-mini"], "paused": false, "last_error": "", "paired_at": "2026-10-01T09:12:00Z"
    },
    {
      "id": "b_c20e44", "saas_name": "Beta Notes", "issuer": "https://lmbp.beta.example", "tenant": "t_9921",
      "state": "revoked", "connected": false, "running_tasks": 1, "max_concurrency": 1, "allow": [],
      "last_error": "device credential rejected (revoked or unknown); pair again"
    }
  ],
  "connection_id": "b_7f3a91",
  "connected": true, "remote_connected": true, "gateway_connected": true,
  "issuer": "https://gw.acme.example", "saas_name": "Acme Chat", "tenant": "acme",
  "device_id": "dev_01HZX8", "server_url": "https://gw.acme.example", "remote_url": "https://chat.acme.example",
  "transport": "wss", "e2ee": true, "last_error": ""
}
```

- `connections[].state` is one of:
  - `online`
  - `connecting`
  - `disconnected` (paused by the user; the pairing is kept)
  - `revoked` (the gateway rejected the device credential)
  - `error` (the key file could not be opened, or a broken binding — see
    below; `last_error` says why. It can always be revoked and re-paired.)
  - `unknown` (UI only: the last status refresh failed, see `stale`)
- `allow` uses `adapter/model`, `adapter/*` or `*`. An empty list denies
  everything. New pairings start empty, and the user picks models on the
  consent screen.
- **Transition:** the flat fields (`connected`, `issuer`, `tenant`, …) mirror
  the **first** connection, so older UI code keeps working. New code reads
  `connections[]`. The flat fields will be removed after the UI migration.
- The status bar shows `已连接 n/m`, where n is `connections_online` and m is
  `connections_total`.

### Status fields added by the review fixes

| field | meaning |
|---|---|
| `stale` (UI-side) | Set by the desktop when a status refresh fails. Every connection is shown as `unknown` / 状态未知 and `connected=false` until a refresh succeeds. Never sent by the kernel. |
| `cleanup_pending[]` | Data-dir relative paths of private-key files of **removed** connections that are still on disk (`bindings/<id>`, and `device_key.enc.v1.bak` for a migrated binding). Persisted in `identity.json` as `pending_cleanup` and retried at every start. |
| `cleanup_errors[]` | Why those deletions (and orphan key dirs) failed last time. |
| `warning` | Present while `cleanup_pending` or orphan leftovers exist; stays until a retry succeeds. |
| `storage_warning` | The last `identity.json` write was committed and verified, but the directory fsync failed (durability not guaranteed). |
| `adapter_storage_warning` | Same for `adapters.enc`. If an `adapters.enc` save cannot be verified, the previous content is written back. If that also fails, adapter changes are refused until restart. |
| `connection_revoke` reply `removed: true` | The connection is gone. A `warning` in the same reply means the key cleanup failed; the UI shows two results (已移除连接 / 本机私钥清理失败). |

**Broken bindings.** A binding whose key cannot be loaded (e.g. a v1
migration whose `device_key.enc` no longer exists) is kept in `connections[]`
with `state: "error"` instead of blocking startup. Only a missing file
(`ENOENT`) is treated that way; a permission or I/O error while reading the
v1 key aborts the migration without changing anything, and the next start
retries it.

## Actions

| action | parameters | effect |
|---|---|---|
| `pair_start` / `pair_prepare` | `remote_url` or `link` (lmbp://pair) | Verifies the gateway's signed metadata. Returns `{pair_id, consent, available_models}`. Allowed while other SaaS are paired. A second binding to the same gateway + tenant is rejected. |
| `pair_approve` | `pair_id`, `allow[]`, `max_concurrency?` | Takes the user's consent and model choice, then returns `user_code` |
| `pair_deny` / `pair_cancel` / `pair_status` | `pair_id` (default: latest) | |
| `connections_list` | | `{connections:[…]}` |
| `connection_disconnect` / `connection_connect` | `connection_id` | Pauses or resumes one SaaS; keys are kept |
| `connection_revoke` (legacy: `disconnect`) | `connection_id` (optional when exactly one) | Stops that session and deletes its local device keys. Other SaaS keep running. |
| `connection_policy_set` | `connection_id`, `policy:{allow?, max_concurrency?}` | **Patch:** only the fields present change (allow-only keeps `max_concurrency`, and vice versa). The UI sends `allow` only and keeps wildcards, disabled-adapter models and unknown entries it did not touch. A concurrency change applies to the running session (`models.update.max_concurrency`) without reconnecting and is used by every later hello; a change made during a reconnect handshake is re-sent after the welcome. |
| `pair_approve` failure | The consent request stays retryable: `pair_status` keeps `state: "consent"` with the same `pair_id`, and `pair_approve` can be sent again. While the request is in flight the state is `approving`. The UI retries on the same page, or restarts pairing when the state is not `consent`. |
| `usage_stats` / `audit_list` | `filter{gateway, saas_name, tenant, connection_id, …}`, or a top-level `connection_id` | `group_by: ["saas"]` gives one bucket per gateway + tenant |

## Limits

- The global `max_concurrency` (default 4) is shared by all connections.
- The per-connection cap is `policy.max_concurrency`.
- **Fair share:** with n active connections, each connection is guaranteed
  max(1, global/n) slots. A connection can go above its share only while the
  slots still owed to the other connections stay free.
- Running tasks are keyed by `(connection id, task id)`. Two gateways can
  use the same `task_id` without colliding.

## Storage (`identity.json` v2)

```json
{"version":2,"data_key":"…","journal_key":"…","bindings":[
  {"id":"b_7f3a91","device_id":"dev_01HZX8","issuer":"https://gw.acme.example","tenant":"acme","saas_name":"Acme Chat",
   "remote_url":"https://chat.acme.example","key_file":"bindings/b_7f3a91/device_key.enc","paired_at":"…","policy":{"allow":["ollama/*"]}}]}
```

**Migration from v1.** A v1 install (`device_id` plus `device_key.enc`) is
migrated on the first start:
1. `identity.json` is copied to `identity.json.v1.bak`.
2. `device_key.enc` is copied to `device_key.enc.v1.bak`.
3. The key is copied to the new binding file and verified byte for byte.
4. The new `identity.json` is written, and the v1 `device_key.enc` is removed.
   The backup copy stays.

The migrated binding keeps serving every local model (`allow: ["*"]`), as it
did before. Revoking that binding also deletes the v1 key backup.

## Commit order and failures

Every change is written to `identity.json` first and applied to the running
kernel only after the write. `durable.Write` reports how far it got:

- **not committed** (failure before or at the rename): the old file is intact.
  The change is not applied. A new pairing's key file is deleted, and a
  revoke releases the connection again.
- **committed, directory sync failed**: the file is read back. If it holds
  the new state, the write counts as committed and `storage_warning` is set.
  If it holds the old state, the write counts as not committed.
- **uncertain** (the read-back fails or shows neither state): nothing
  destructive happens.
  - A pairing keeps its key file; at the next start the disk decides, and
    orphan cleanup removes the key if it is unreferenced.
  - A revoke leaves the connection closing (no new tasks) and keeps its
    keys. Retrying the revoke completes it.

After an uncertain write the kernel **reconciles before any further change**
(pairing, policy, pause/connect, revoke): it re-reads `identity.json` and
makes its in-memory bindings match the disk, so a later change can never
write back a binding the disk already dropped. While the file cannot be read,
every change is refused with an error. A connection closed by an uncertain
revoke stays closed: a retry that fails before writing does not reopen it,
and if reconcile shows the revoke landed, the retry reports `removed: true`.
The v1 migration uses the same phases: it never deletes a key copy that a
committed v2 file may reference.

`models.update` announcements are serialized: the config is read and sent
under one lock, so a stale concurrency value cannot overtake a newer one.

The cleanup `warning` is shown in the app's shared outer layout, so it stays
visible on the pairing page after the last connection is revoked.

Order per action:
- **Pairing:** key file → `identity.json` → start link. If the link fails, the
  binding is removed again. If that rollback write also fails, the binding
  stays as a broken binding.
- **Revoke:** stop admitting tasks (refused while tasks run) → `identity.json`
  without the binding and with its key paths in `pending_cleanup` → stop the
  session and wait for task cleanup → delete files → drop the deleted paths
  from `pending_cleanup`.
- **Pause and policy:** `identity.json` → apply. If applying fails, the old
  state is written back. If that rollback write fails too, the error says so;
  runtime state matches the disk, and the next start applies the saved
  setting.

On a remote revoke (credential rejected) or a kernel stop, the connection's
in-flight tasks are cancelled. A transient disconnect keeps them for resume.

## Threading

- `status` and `list_adapters` are polled from a background isolate, never
  on the UI thread.
- `test_adapter`, `codex_models`, `create_adapter` / `update_adapter` (model
  probes) and the network part of `pair_approve` run without the kernel's
  global lock, so status stays responsive while they wait.
- `stop` closes the audit log. The next `start` opens `audit.db` in its own
  `data_dir`.

## Usage and audit

- `group_by: ["connection"]` returns one bucket per gateway + tenant + device
  id (`connection`, `connection_gateway`, `connection_tenant`). Device ids are
  only unique per gateway.
- `e2ee` in audit records is true for tasks that arrived end-to-end encrypted,
  even though the run is decrypted before it executes.

## Scheme (adapter) names

- Names are unique among schemes, compared trimmed and case-insensitively. `adapter_create` / `adapter_update` (full or partial) reject a name used by another scheme with an error containing `scheme name already exists`; a scheme may keep its own name in any case.
- An empty name gets the default: the first model itself when free (e.g. `gpt-6.1-sol`), otherwise `<model>_<n>` with the smallest free `n >= 2` (`gpt-6.1-sol_2`, `gpt-6.1-sol_3`, …). The desktop editor shows the same default for a new scheme and keeps it in step with the chosen model until the user types a name.
- Older `adapters.enc` files with repeated names still load; later duplicates (by id) are shown as `name (2)`, `name (3)`, … and that name is saved on the next adapter change.
- Display only: the connection model dialog, consent checklist and connection tile show scheme names (`schemeLabels`, model as secondary text); `allow` keeps the `adapter/model` ids. The device advertises names to gateways as `model_names` (lmbp-spec §16.1); requests still use the ids.
