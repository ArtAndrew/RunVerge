# SPDX-License-Identifier: Elastic-2.0
"""Test-only schema tools; no SDK imports and no network reference resolution."""
from __future__ import annotations

import json
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
sys.path.insert(0, str(HERE / ".deps"))

from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry
from referencing.exceptions import NoSuchResource


def read_json(path: Path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate JSON property in {path.name}")
            result[key] = value
        return result

    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique)


def no_external_references(uri):
    raise NoSuchResource(ref=uri)


def validator(schema):
    return Draft202012Validator(
        schema, format_checker=FormatChecker(),
        registry=Registry(retrieve=no_external_references),
    )


def dto_validator(document, name):
    return validator({
        "$schema": document["$schema"], "$defs": document["$defs"],
        "$ref": f"#/$defs/{name}",
    })


def response_validator(openapi, route, method, status):
    response = openapi["paths"][route][method]["responses"][str(status)]
    if "$ref" in response:
        response = resolve_pointer(openapi, response["$ref"])
    schema = response["content"]["application/json"]["schema"]
    return validator({**schema, "components": openapi["components"]})


def resolve_pointer(document, reference):
    if not reference.startswith("#/"):
        raise AssertionError("Only local contract references are permitted")
    value = document
    for key in reference[2:].split("/"):
        value = value[key.replace("~1", "/").replace("~0", "~")]
    return value


def references(value):
    if isinstance(value, dict):
        if "$ref" in value:
            yield value["$ref"]
        for child in value.values():
            yield from references(child)
    elif isinstance(value, list):
        for child in value:
            yield from references(child)
