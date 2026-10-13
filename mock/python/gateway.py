#!/usr/bin/env python3
"""LSP2 mock SaaS gateway (Python): a thin CLI over the lsp2-server SDK
(sdk/python), served by uvicorn. All protocol behaviour lives in the SDK;
this file only parses flags, registers tenants and serves the shared chat
page (mock/web)."""
from __future__ import annotations

import argparse
import json
import os
import sys

import uvicorn

from lsp2_server import LSP2Server, SQLiteStore, TenantConfig
from lsp2_server.engine import WEB_CSP, WEB_FILES, Gateway  # noqa: F401  (re-exported for tests)
from lsp2_server.protocol import MAX_FRAME_BYTES


def build(argv=None):
    ap = argparse.ArgumentParser(description="LSP2 mock SaaS gateway (Python)")
    ap.add_argument("--listen", default=os.environ.get("LSP2_LISTEN", "127.0.0.1:8080"))
    ap.add_argument("--issuer", default=os.environ.get("LSP2_ISSUER", ""),
                    help="gateway issuer origin (https; http only for loopback). Default: request origin (dev mode)")
    ap.add_argument("--tenant", action="append", default=[], metavar="ID:ENV",
                    help="register tenant ID whose API key (>= 32 chars) is read from env var ENV")
    ap.add_argument("--tenant-profiles", default=os.environ.get("LSP2_TENANT_PROFILES", ""),
                    help='JSON {"<tenant>": {"api_key_env": "...", "display_name": "...", "requested_scopes": [...], ...}}')
    ap.add_argument("--signing-key-env", default="LSP2_SIGNING_SEED", help="env var holding a hex Ed25519 seed (ephemeral if unset)")
    ap.add_argument("--max-concurrency", type=int, default=16)
    ap.add_argument("--db", default=os.environ.get("LSP2_DB", ""), help="persist state in SQLite (default: memory)")
    ap.add_argument("--web-dir", default=os.environ.get("LSP2_WEB_DIR", os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "web")),
                    help="directory with the shared chat page, served at / and /chat")
    ap.add_argument("--no-chat-ui", action="store_true", help="do not serve the chat page")
    a = ap.parse_args(argv)
    seed_hex = os.environ.get(a.signing_key_env, "")
    if not seed_hex and not a.db:
        print("warning: no signing key; using an ephemeral key (tokens do not survive restarts)", file=sys.stderr)
    tenants = []
    for spec in a.tenant:
        tid, _, env = spec.partition(":")
        if not os.environ.get(env):
            sys.exit(f"tenant {tid}: env {env} is empty")
        tenants.append(TenantConfig(tid, os.environ[env]))
    if a.tenant_profiles:
        with open(a.tenant_profiles) as f:
            for tid, p in json.load(f).items():
                key = os.environ.get(p.get("api_key_env", ""), "")
                if not key:
                    sys.exit(f"tenant {tid}: api_key_env {p.get('api_key_env')} is empty")
                tenants.append(TenantConfig(tid, key, p))
    if not tenants:
        print("warning: no tenants registered (use --tenant id:ENV_VAR)", file=sys.stderr)
    lsp = LSP2Server(tenants, issuer=a.issuer, signing_seed=bytes.fromhex(seed_hex) if seed_hex else None,
                     max_concurrency=a.max_concurrency, chat_ui_dir="" if a.no_chat_ui else a.web_dir,
                     store=SQLiteStore(a.db) if a.db else None)
    return a, lsp


def main():
    a, lsp = build()
    host, _, port = a.listen.rpartition(":")
    print(f"lsp2 python mock gateway listening on {a.listen} issuer={lsp.engine.cfg['issuer'] or '(request origin, dev mode)'} kid={lsp.keys.kid()}",
          file=sys.stderr, flush=True)
    uvicorn.run(lsp.asgi(), host=host or "0.0.0.0", port=int(port), log_level="warning", ws="websockets",
                ws_max_size=MAX_FRAME_BYTES, ws_ping_interval=20, ws_ping_timeout=20)


if __name__ == "__main__":
    main()
