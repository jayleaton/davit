"""Run results."""

from __future__ import annotations

from dataclasses import dataclass

from .models import PushResult, Run

__all__ = ["RunOutcome"]


@dataclass(frozen=True)
class RunOutcome:
    """Result of :meth:`davit.Client.run_to_completion`.

    Attributes:
        run: The ended run (``state`` succeeded, failed or cancelled).
        exit_code: Shell-style status like ``davit run``: 130 when cancelled,
            124 for its timeout, the command's code (``& 0xff``), 128+n for the
            signal that killed it (``signal``), else 255. Non-zero is not an
            exception.
        stdout: Everything the command wrote to stdout.
        stderr: Everything the command wrote to stderr.
        push: What was pushed before the command (``None`` without ``files``).
        signal: The signal that killed the command (``run.signal``), if any.
    """

    run: Run
    exit_code: int
    stdout: bytes
    stderr: bytes
    push: PushResult | None = None
    signal: str | None = None
