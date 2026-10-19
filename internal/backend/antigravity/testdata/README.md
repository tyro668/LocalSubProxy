# agy stream-json samples

None of these were recorded from a real `agy` run (no signed-in Antigravity
CLI was available). They are **doc-derived** from
<https://antigravity.google/docs/cli/headless/>. The fake CLI replaces the
documented `cwd` `/home/user/project` with the private workspace.

| File | Source |
|---|---|
| `doc-stream.jsonl` | Verbatim "Streaming JSON" example. Its `init` names no agent, so Avora refuses it. |
| `composed-stream.jsonl` | The same example, `init` with `agent` and `model` as documented for `--agent`/`--model`. |
| `composed-chunks.jsonl` | The first turn of the stdin example (ACTIVE delta, then DONE), under the composed `init`. |
| `composed-tool.jsonl` | The composed `init` plus the documented `run_command` tool step. |

Replace them with recorded output when a real run is possible.
