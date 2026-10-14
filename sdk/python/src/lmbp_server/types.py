"""Request shapes of the in-process client (spec/openapi.yaml ChatRequest and
MessagesRequest). Pass them as keyword arguments, e.g.
``client.chat.completions.create(**req)``; tests/test_types.py keeps the keys
in step with the spec."""
from __future__ import annotations

from typing import List, Literal, TypedDict, Union


class ChatRequest(TypedDict, total=False):
    model: str
    messages: List[dict]
    tools: List[dict]
    tool_choice: Union[str, dict]
    stream: bool
    max_tokens: int
    max_completion_tokens: int  # newer OpenAI alias of max_tokens
    temperature: float
    top_p: float
    stop: Union[str, List[str]]  # up to 4
    n: Literal[1]
    lmbp_e2ee: str  # sealed request; messages/tools must then be absent


class MessagesRequest(TypedDict, total=False):
    model: str
    max_tokens: int
    stream: bool
    temperature: float
    top_p: float
    stop_sequences: List[str]  # up to 4
    system: Union[str, List[dict]]
    messages: List[dict]
    tools: List[dict]
    tool_choice: dict
