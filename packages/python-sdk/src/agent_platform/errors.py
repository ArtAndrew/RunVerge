# SPDX-License-Identifier: Apache-2.0
"""Stable exceptions exposed by the Python SDK."""

from __future__ import annotations


class AgentPlatformError(Exception):
    """Base class for failures originating from the SDK or API."""


class APIError(AgentPlatformError):
    """An HTTP error returned by the server.

    ``retryable`` is advisory only. The SDK never automatically retries requests.
    Reuse the same idempotency key when retrying a mutation.
    """

    def __init__(
        self,
        *,
        status_code: int,
        code: str,
        message: str,
        request_id: str | None = None,
        retryable: bool = False,
    ) -> None:
        self.status_code = status_code
        self.code = code
        self.message = message
        self.request_id = request_id
        self.retryable = retryable
        super().__init__(f"{code}: {message} (HTTP {status_code})")


class TransportError(AgentPlatformError):
    """The request failed before a valid HTTP response was available.

    A failed connection does not prove that a mutation was not committed. Check
    the task or retry with the original idempotency key before creating new work.
    """


class ProtocolError(AgentPlatformError):
    """The server response did not conform to the JSON API contract."""
