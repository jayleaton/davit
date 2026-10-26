"""Prompt results."""

from __future__ import annotations

from dataclasses import dataclass, field

from .models import Prompt, PromptEvent

__all__ = ["PromptResult"]


@dataclass(frozen=True)
class PromptResult:
    """Result of :meth:`davit.Client.run_prompt`.

    Attributes:
        prompt: The final turn (check ``state`` and ``error``).
        text: Concatenated assistant ``message.delta`` texts.
        events: Every event of the turn, in order.
    """

    prompt: Prompt
    text: str
    events: list[PromptEvent] = field(default_factory=list)
