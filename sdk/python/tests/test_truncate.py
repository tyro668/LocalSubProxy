"""Third audit item 3: a stream that ends (EOF) before its terminator
([DONE] / message_stop / an error event) is not a normal end. The in-process
client must raise IncompleteStreamError carrying the resume cursor."""
import pytest

import lmbp_server
from lmbp_server.client import InProcessClient, LMBPAPIError


def fake(body: str) -> InProcessClient:
    """A truncating fake server: sends ``body`` (in two pieces) and ends."""
    c = InProcessClient(object(), "t", "u")

    async def _request(method, path, b, extra):
        async def chunks():
            raw = body.encode()
            yield raw[: len(raw) // 2]
            yield raw[len(raw) // 2:]
        return 200, {"content-type": "text/event-stream", "x-task-id": "task_fake"}, chunks()
    c._request = _request
    return c


CHUNK_A = '{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"A"},"finish_reason":null}]}'


async def drain(st):
    text = ""
    try:
        async for c in st:
            if "choices" in c:
                text += c["choices"][0]["delta"].get("content", "")
        return text, None
    except Exception as e:  # noqa: BLE001
        return text, e


def incomplete(err, cursor):
    cls = getattr(lmbp_server, "IncompleteStreamError", None)
    assert cls is not None, "IncompleteStreamError is exported"
    assert isinstance(err, cls), f"want IncompleteStreamError, got {err!r}"
    assert err.code == "INCOMPLETE_STREAM"
    assert err.last_event_id == cursor and err.task_id == "task_fake"


@pytest.mark.parametrize("body,cursor,text", [
    (f"id: 1\ndata: {CHUNK_A}\n\nid: 2\ndata: {CHUNK_A}\n\n", "2", "AA"),
    (f"id: 1\ndata: {CHUNK_A}\n\nid: 2\ndata: {CHUNK_A}", "1", "A"),
    ("", "", ""),
    (f'id: 1\ndata: {CHUNK_A}\n\nid: 2\ndata: {{"choices":[{{"index":0,"delta":{{}},"finish_reason":"stop"}}]}}\n\n', "2", "A"),
], ids=["eof-after-two-chunks", "eof-mid-event", "eof-before-any-event", "eof-before-DONE"])
async def test_chat_truncated(body, cursor, text):
    st = fake(body).chat.completions.stream(model="m", messages=[])
    got, err = await drain(st)
    assert got == text
    incomplete(err, cursor)
    assert st.last_event_id == cursor


async def test_chat_done_is_normal_end():
    got, err = await drain(fake(f"id: 1\ndata: {CHUNK_A}\n\ndata: [DONE]\n\n").chat.completions.stream(model="m", messages=[]))
    assert err is None and got == "A"


async def test_chat_error_event_is_api_error():
    _, err = await drain(fake('id: 1\ndata: {"error":{"code":"DEVICE_OFFLINE","message":"x"}}\n\n').chat.completions.stream(model="m", messages=[]))
    assert isinstance(err, LMBPAPIError) and err.code == "DEVICE_OFFLINE"


HEAD = ('event: message_start\ndata: {"type":"message_start","message":{}}\n\n'
        'event: content_block_delta\nid: 1\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}\n\n')


async def test_messages_truncated():
    _, err = await drain(fake(HEAD + 'event: ping\ndata: {"type":"ping"}\n\n').messages.stream(model="m", max_tokens=8, messages=[]))
    incomplete(err, "1")


async def test_messages_stop_is_normal_end():
    _, err = await drain(fake(HEAD + 'event: message_stop\nid: 2\ndata: {"type":"message_stop"}\n\n').messages.stream(model="m", max_tokens=8, messages=[]))
    assert err is None
