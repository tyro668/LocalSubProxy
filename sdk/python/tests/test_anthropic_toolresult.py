"""Review9 P2-10: tool_result images are converted (image_url parts of the
next user message), unsupported blocks are a clear error, never dropped."""
import pytest

from lmbp_server.anthropic import to_openai


def conv(content):
    return {"model": "m", "max_tokens": 8, "messages": [
        {"role": "user", "content": "take a screenshot"},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "screenshot", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": content}]},
    ]}


def test_tool_result_image_converted():
    out = to_openai(conv([{"type": "text", "text": "here"}, {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}}]))
    tool = next(m for m in out["messages"] if m["role"] == "tool")
    assert "here" in tool["content"]
    last = out["messages"][-1]
    assert last["role"] == "user"
    assert any(p.get("type") == "image_url" and p["image_url"]["url"] == "data:image/png;base64,QUJD" for p in last["content"]), out["messages"]


def test_tool_result_unsupported_rejected():
    with pytest.raises(ValueError, match="tool_result content type document"):
        to_openai(conv([{"type": "document", "source": {"type": "text", "data": "x"}}]))
