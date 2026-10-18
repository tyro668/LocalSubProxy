# Python LMBP 模拟网关

`gateway.py` 是 LMBP（[docs/lmbp-spec.md](../../docs/lmbp-spec.md)）网关的 Python 命令行外壳：协议实现全部在服务端 SDK [`sdk/python`](../../sdk/python/README.md)（包名 `lmbp-server`，ASGI）里，这里只解析参数并用 uvicorn 运行 `LMBPServer(...).asgi()`。需要 Python ≥ 3.10。

## 运行

> 一键启动（假模型 + 网关 + 聊天页，并打印登录信息和配对命令）：`scripts/mock-start.sh python`，停止用 `scripts/mock-stop.sh`。详见 [mock/README.md](../README.md)。

```bash
cd mock/python
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
export LMBP_TENANT_KEY_ACME=$(openssl rand -hex 24)   # 租户 API Key（≥ 32 字符）
export LMBP_SIGNING_SEED=$(openssl rand -hex 32)     # 可选：固定网关签名密钥
.venv/bin/python gateway.py --listen 127.0.0.1:8080 --tenant acme:LMBP_TENANT_KEY_ACME
# 或带 SaaS 展示身份：
.venv/bin/python gateway.py --listen 127.0.0.1:8080 --tenant-profiles ../tenant-profiles.example.json
```

参数与 Node 版相同：`--listen`、`--issuer`、`--tenant id:ENV`（可重复）、`--tenant-profiles`、`--signing-key-env`（默认读 `LMBP_SIGNING_SEED`）、`--db`（SQLite 持久化，默认内存）、`--max-concurrency`、`--web-dir`（聊天页目录，默认 `../web`，见 [../web/README.md](../web/README.md)）、`--no-chat-ui`；对应环境变量 `LMBP_LISTEN`、`LMBP_ISSUER`、`LMBP_TENANT_PROFILES`。

Docker：

```bash
docker build -f mock/python/Dockerfile -t lmbp-mock-python .   # 在仓库根目录构建，镜像内含聊天页
docker run --rm -p 8080:8080 -e LMBP_TENANT_KEY_ACME -e LMBP_SIGNING_SEED lmbp-mock-python \
  --issuer http://127.0.0.1:8080 --tenant acme:LMBP_TENANT_KEY_ACME
```

单元测试：`.venv/bin/python -m unittest test_web`（聊天页与 CSP）；协议单元测试在 `sdk/python`（`cd sdk/python && .venv/bin/pytest`）。

## 实现要点

- `gateway.py`：CLI（参数、租户文件、`SQLiteStore`），用 uvicorn 运行（`--ws websockets`，以便握手阶段返回 401/403 Problem Details）。
- 协议引擎（每个 WebSocket 连接一个写协程保证 seq 顺序；客户端断开即取消处理协程并发出 `task.cancel`）、Store、配对 API、兼容层：见 [`sdk/python`](../../sdk/python/README.md)。

## 功能

与 Go 参考网关行为一致（差异见 [docs/implementations.md](../../docs/implementations.md)）。协议能力和 API 见 [`sdk/python`](../../sdk/python/README.md)，接口细节以 [规范](../../docs/lmbp-spec.md) 为准。默认状态在内存中，重启后需重新配对；加 `--db` 后持久化到 SQLite。

## 验证

```bash
# 在仓库根目录：对 Node、Python 和 Go 参考网关分别跑 lmbp-conformance 和 Go 连接器端到端测试
scripts/mock-interop.sh node node-sqlite python python-sqlite go sdk-go
```

端到端测试（`mock/external`）用真实的桌面连接器通过 `lmbp://pair` 链接配对（校验签名元数据、本地拒绝、SaaS 拒绝），再跑流式对话、tools、40 KiB 图片、E2EE、本地审计日志和吊销。
