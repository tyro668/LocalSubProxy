# Grok streaming-json samples

None of these were recorded from a real `grok` run (no signed-in Grok CLI
was available). They are **doc-derived**:

| File | Source |
|---|---|
| `doc-tool-call.jsonl` | Verbatim from grok-build `docs/user-guide/14-headless-mode.md`, "streaming-json" (`usage:{...}` placeholders replaced by `{}`). |
| `doc-readme.jsonl` | Verbatim from the grok-build README "streaming-json" example (older `EndTurn` spelling). |
| `composed-text.jsonl` | Composed: the documented line shapes, plus the `available_commands` line from `reducer/acp.rs`, without the tool call. |
| `composed-readme-endturn.jsonl` | Composed: the README example with an `available_commands` line first. |

Replace them with recorded output when a real run is possible.
