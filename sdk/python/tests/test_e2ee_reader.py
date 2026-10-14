"""B3: per-frame open() is stateless; the response reader enforces seq order
(last+1) and the sealed task.done terminator."""
import json

import pytest

from lmbp_server.e2ee import E2EESealed, seal_chunk

KEY, RID = bytes([7]) * 32, "rid_test"
PTS = [("task.delta", '{"text":"a"}'), ("task.delta", '{"text":"b"}'), ("task.done", '{"finish_reason":"stop"}')]
F = [{"seq": i + 1, "type": t, "data": seal_chunk(KEY, RID, t, i + 1, p.encode())} for i, (t, p) in enumerate(PTS)]
S = E2EESealed("", RID, KEY)


def read_all(frames, after=0):
    r = S.reader(after)
    out = ""
    for f in frames:
        p = r.next(f)
        if f["type"] == "task.delta":
            out += json.loads(p)["text"]
    r.finish()
    return out


def test_open_alone_is_stateless():
    for i in (1, 0, 0, 2):
        S.open(F[i])


def test_reader_enforces_order_and_terminator():
    assert read_all(F) == "ab"
    assert read_all(F[1:], 1) == "b"
    cases = {"reordered": [F[1], F[0], F[2]], "duplicated": [F[0], F[0], F[1], F[2]], "dropped middle": [F[0], F[2]],
             "truncated": [F[0], F[1]], "after done": [F[0], F[1], F[2], F[2]], "type swapped": [F[0], F[1], {**F[2], "type": "task.delta"}]}
    for name, frames in cases.items():
        with pytest.raises(ValueError, match="e2ee"):
            read_all(frames)
