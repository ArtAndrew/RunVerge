# SPDX-License-Identifier: Apache-2.0
"""Exact parsing for contract 0.3 decimal-string financial facts."""

import re

Money = str
_CANONICAL = re.compile(r"(?:0|-?[1-9][0-9]*)", re.ASCII)
_MAX = (1 << 63) - 1
_MIN = -(1 << 63)


def parse_micro_usd(value: Money, *, signed: bool = False) -> int:
    """Parse an exact int64 micro-USD string; allow negatives only for deltas.

    JSON numbers, whitespace, fractions, exponents, leading zeros and ``-0``
    are rejected. This function never converts through a floating-point value.
    """
    if not isinstance(value, str):
        raise TypeError("micro-USD must be a canonical decimal string")
    if len(value) > 20 or _CANONICAL.fullmatch(value) is None:
        raise ValueError("micro-USD must be a canonical decimal string")
    parsed = int(value)
    if parsed > _MAX or parsed < (_MIN if signed else 0):
        raise ValueError("micro-USD is outside the permitted int64 range")
    return parsed
