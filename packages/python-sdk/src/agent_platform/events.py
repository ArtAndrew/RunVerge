# SPDX-License-Identifier: Apache-2.0
"""Bounded event contract validation and one-connection SSE parsing."""
from __future__ import annotations

import json
import re
from datetime import datetime
from typing import Any

from .errors import ProtocolError
from .event_payloads import EVENT_PAYLOAD_REGISTRY

API_VERSION = "0.14.0"
API_VERSION_HEADER = "X-Platform-API-Version"
MAX_EVENT_FRAME_BYTES = 65_536


def _fail() -> None:
    raise ProtocolError("Event response does not match the supported contract")


def sequence(value: Any, allow_zero: bool = False) -> str:
    if (not isinstance(value, str) or not re.fullmatch(r"0|[1-9][0-9]*", value)
            or len(value) > 19 or (len(value) == 19 and value > "9223372036854775807")
            or (value == "0" and not allow_zero)):
        _fail()
    return value


def cursor(value: Any, allow_empty: bool = False) -> str:
    if not isinstance(value, str) or len(value) > 1024 or (not value and not allow_empty) or (value and not re.fullmatch(r"[A-Za-z0-9_-]+", value)):
        _fail()
    return value


def check_version(headers: Any) -> None:
    values = headers.get_all(API_VERSION_HEADER) if callable(getattr(headers, "get_all", None)) else [headers.get(API_VERSION_HEADER)]
    if values != [API_VERSION]:
        raise ProtocolError("Incompatible API wire version; commands must not be retried automatically")


def _exact(value: Any, fields: set[str]) -> None:
    if not isinstance(value, dict) or set(value) - fields:
        _fail()


def _timestamp(value: Any) -> bool:
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:[0-9]{2})", value):
        return False
    if value[-1] != "Z" and (int(value[-5:-3]) > 23 or int(value[-2:]) > 59):
        return False
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).tzinfo is not None
    except ValueError:
        return False


def event(value: Any, organization_id: str, task_id: str) -> dict[str, Any]:
    if not isinstance(value, dict) or not isinstance(value.get("id"), str) or not value["id"] or value.get("organization_id") != organization_id or value.get("task_id") != task_id or not isinstance(value.get("type"), str) or not value["type"] or not _timestamp(value.get("created_at")):
        _fail()
    sequence(value.get("sequence"))
    fields = {"id", "organization_id", "task_id", "sequence", "type", "created_at", "payload_version"}
    version = value.get("payload_version")
    if type(version) is not int:
        _fail()
    if version == 0:
        _exact(value, fields | {"legacy_data_json"})
        if not isinstance(value.get("legacy_data_json"), str) or not value["legacy_data_json"]:
            _fail()
    elif version == 1:
        _exact(value, fields | {"data"})
        if not isinstance(value.get("data"), dict):
            _fail()
        _payload(value["type"], value["data"])
    else:
        _fail()
    return value


def _payload(kind: str, data: dict[str, Any]) -> None:
    definition = EVENT_PAYLOAD_REGISTRY["types"].get(kind)
    if definition is None or set(data) - set(definition["fields"]):
        _fail()
    for name, field in definition["fields"].items():
        if name not in data:
            if field["required"]:
                _fail()
            continue
        value = data[name]
        if field["kind"] == "boolean":
            if type(value) is not bool:
                _fail()
        elif field["kind"] == "integer":
            if type(value) is not int or not field.get("minimum", 0) <= value <= field.get("maximum", 2147483647):
                _fail()
        elif field["kind"] == "decimal":
            if not field.get("minimum", 0) <= int(sequence(value, True)) <= int(field.get("maximum_decimal", "9223372036854775807")):
                _fail()
        else:
            if not isinstance(value, str) or (not field.get("allow_empty") and not value.strip()) or "\0" in value:
                _fail()
            try:
                if len(value.encode("utf-8")) > field.get("max_bytes", 65536):
                    _fail()
            except UnicodeEncodeError:
                _fail()
            if field["kind"] == "digest" and not re.fullmatch(r"[0-9a-f]{64}", value):
                _fail()
            if field["kind"] == "timestamp" and not _timestamp(value):
                _fail()
        if "const" in field and (type(value) is not type(field["const"]) or value != field["const"]):
            _fail()
        if "enum" in field and value not in field["enum"]:
            _fail()
    variants = definition.get("variants")
    if variants:
        matched = False
        for variant in variants:
            if data.get(variant["field"]) != variant["value"]:
                continue
            matched = True
            if any(name not in data for name in variant.get("required", [])) or any(name in data for name in variant.get("forbidden", [])):
                _fail()
        if not matched:
            _fail()


def _window(value: Any) -> dict[str, Any]:
    _exact(value, {"after_sequence", "through_sequence", "version"})
    if type(value.get("version")) is not int or not 1 <= value["version"] <= 2147483647 or int(sequence(value.get("after_sequence"), True)) > int(sequence(value.get("through_sequence"), True)):
        _fail()
    return value


def _events(value: Any, organization_id: str, task_id: str, window: dict[str, Any] | None = None) -> list[dict[str, Any]]:
    if not isinstance(value, list) or len(value) > 100:
        _fail()
    ids: set[str] = set()
    previous = 0
    for item in value:
        event(item, organization_id, task_id)
        current = int(item["sequence"])
        if item["id"] in ids or current <= previous or (window and not int(window["after_sequence"]) < current <= int(window["through_sequence"])):
            _fail()
        ids.add(item["id"])
        previous = current
    return value


def batch(value: Any, organization_id: str, task_id: str) -> dict[str, Any]:
    _exact(value, {"organization_id", "task_id", "events", "cursor", "has_more", "reset_required", "reset_reason", "window"})
    if value.get("organization_id") != organization_id or value.get("task_id") != task_id or type(value.get("has_more")) is not bool or type(value.get("reset_required")) is not bool:
        _fail()
    window = _window(value.get("window"))
    items = _events(value.get("events"), organization_id, task_id, window)
    if value["reset_required"]:
        if items or value.get("cursor") != "" or value.get("reset_reason") != "history_expired" or value["has_more"]:
            _fail()
    else:
        cursor(value.get("cursor"))
        if "reset_reason" in value:
            _fail()
    return value


def snapshot(value: Any, organization_id: str, task_id: str) -> dict[str, Any]:
    _exact(value, {"organization_id", "task_id", "task", "events", "cursor", "replay_cursor", "window", "has_earlier_events", "observed_at"})
    task = value.get("task")
    if value.get("organization_id") != organization_id or value.get("task_id") != task_id or not isinstance(task, dict) or task.get("organization_id") != organization_id or task.get("id") != task_id or type(value.get("has_earlier_events")) is not bool or not _timestamp(value.get("observed_at")):
        _fail()
    cursor(value.get("cursor")); cursor(value.get("replay_cursor"))
    sequence(task.get("reserved_microusd"), True); sequence(task.get("spent_microusd"), True)
    window = _window(value.get("window"))
    if len(_events(value.get("events"), organization_id, task_id, window)) > 50:
        _fail()
    return value


def _json(text: str) -> Any:
    def pairs(values: list[tuple[str, Any]]) -> dict[str, Any]:
        result = {}
        for key, value in values:
            if key in result:
                _fail()
            result[key] = value
        return result
    def number(value: str) -> int:
        if len(value) > 16 or not re.fullmatch(r"-?(0|[1-9][0-9]*)", value):
            _fail()
        result = int(value)
        if abs(result) > 9_007_199_254_740_991:
            _fail()
        return result
    def unsupported(_value: str) -> Any:
        _fail()
    try:
        value = json.loads(text, object_pairs_hook=pairs, parse_int=number, parse_float=unsupported, parse_constant=unsupported)
        nodes = 0
        def bounded(item: Any, depth: int) -> None:
            nonlocal nodes
            nodes += 1
            if nodes > 10_000 or depth > 32:
                _fail()
            if isinstance(item, (list, dict)):
                for child in item.values() if isinstance(item, dict) else item:
                    bounded(child, depth + 1)
        bounded(value, 0)
        return value
    except (ValueError, RecursionError) as exc:
        raise ProtocolError("SSE frame is not valid bounded JSON") from exc


class EventStreamParser:
    """Strict platform subset of SSE; incomplete frames never advance a cursor."""
    def __init__(self, organization_id: str, task_id: str) -> None:
        self.organization_id, self.task_id = organization_id, task_id
        self.line = bytearray()
        self.frame_bytes = 0
        self.skip_lf = False
        self.event: str | None = None
        self.id: str | None = None
        self.data: list[str] = []

    def push(self, chunk: bytes) -> list[dict[str, Any]]:
        frames = []
        for char in chunk:
            if self.skip_lf:
                self.skip_lf = False
                if char == 10:
                    if self.frame_bytes:
                        self.frame_bytes += 1
                        if self.frame_bytes > MAX_EVENT_FRAME_BYTES:
                            raise ProtocolError("SSE frame exceeds 64 KiB")
                    continue
            self.frame_bytes += 1
            if self.frame_bytes > MAX_EVENT_FRAME_BYTES:
                raise ProtocolError("SSE frame exceeds 64 KiB")
            if char == 13 and not self.line and self.frame_bytes == MAX_EVENT_FRAME_BYTES:
                raise ProtocolError("SSE frame exceeds 64 KiB including CRLF")
            if char in (10, 13):
                frame = self._line()
                if frame is not None:
                    frames.append(frame)
                self.skip_lf = char == 13
            else:
                self.line.append(char)
        return frames

    def finish(self) -> None:
        if self.line or self.event is not None or self.id is not None or self.data:
            raise ProtocolError("SSE connection ended inside a frame; retain the previous cursor")

    def _line(self) -> dict[str, Any] | None:
        try:
            line = self.line.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ProtocolError("SSE is not valid UTF-8") from exc
        self.line.clear()
        if not line:
            kind, identifier, data = self.event, self.id, self.data
            self.event, self.id, self.data, self.frame_bytes = None, None, [], 0
            if kind is None and identifier is None and not data:
                return None
            if not kind or not data:
                _fail()
            frame = _json("\n".join(data))
            _exact(frame, {"frame_version", "kind", "batch", "error"})
            if type(frame.get("frame_version")) is not int or frame["frame_version"] != 1:
                _fail()
            if kind == "platform.error" and frame.get("kind") == "error":
                if identifier is not None or "batch" in frame:
                    _fail()
                error = frame.get("error")
                _exact(error, {"code", "message", "retryable"})
                if not isinstance(error.get("code"), str) or not error["code"] or not isinstance(error.get("message"), str) or type(error.get("retryable")) is not bool:
                    _fail()
            else:
                value = batch(frame.get("batch"), self.organization_id, self.task_id)
                if "error" in frame:
                    _fail()
                if value["reset_required"]:
                    if kind != "platform.reset" or frame.get("kind") != "reset_required" or identifier is not None:
                        _fail()
                elif kind != "platform.events" or frame.get("kind") != "batch" or identifier != value["cursor"]:
                    _fail()
            return frame
        if line.startswith(":"):
            return None
        field, separator, value = line.partition(":")
        if not separator:
            value = ""
        if value.startswith(" "):
            value = value[1:]
        if field == "data":
            self.data.append(value)
        elif field == "event" and self.event is None:
            self.event = value
        elif field == "id" and self.id is None:
            self.id = cursor(value)
        else:
            _fail()
        return None
