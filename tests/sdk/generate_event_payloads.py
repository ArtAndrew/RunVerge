# SPDX-License-Identifier: Apache-2.0
"""Generate SDK protocol declarations, never server implementation code."""
from __future__ import annotations
import argparse
import json
from pathlib import Path
import pprint

ROOT = Path(__file__).resolve().parents[2]


def generated() -> dict[Path, str]:
    registry = json.loads((ROOT / "contracts/event-payloads.json").read_text())
    assert registry["license"] == "Apache-2.0" and registry["api_version"] == "0.14.0" and registry["registry_version"] == 1
    types = []
    for kind, definition in registry["types"].items():
        fields = []
        for name, field in definition["fields"].items():
            value_type = {"integer": "number", "boolean": "boolean"}.get(field["kind"], "string")
            if "const" in field:
                value_type = json.dumps(field["const"])
            elif "enum" in field:
                value_type = " | ".join(json.dumps(value) for value in field["enum"])
            fields.append(f"{json.dumps(name)}{'' if field['required'] else '?'}: {value_type}")
        variants = []
        for variant in definition.get("variants", []):
            additional = [f"{json.dumps(variant['field'])}: {json.dumps(variant['value'])}"]
            additional += [f"{json.dumps(name)}: string" for name in variant.get("required", [])]
            additional += [f"{json.dumps(name)}?: never" for name in variant.get("forbidden", [])]
            variants.append("{ " + "; ".join(additional) + " }")
        declaration = "{ " + "; ".join(fields) + " }"
        if variants:
            declaration += " & (" + " | ".join(variants) + ")"
        types.append(f"  {json.dumps(kind)}: {declaration};")
    header = "// SPDX-License-Identifier: Apache-2.0\n// Generated from contracts/event-payloads.json; run tests/sdk/generate_event_payloads.py.\n"
    ts = header + "export const EVENT_PAYLOAD_REGISTRY = " + json.dumps(registry, ensure_ascii=False, indent=2) + " as const;\n\nexport interface EventPayloadMap {\n" + "\n".join(types) + "\n}\n"
    py = '# SPDX-License-Identifier: Apache-2.0\n"""Generated public protocol declarations; see tests/sdk/generate_event_payloads.py."""\nfrom typing import Literal, TypedDict\n\nEVENT_PAYLOAD_REGISTRY = ' + pprint.pformat(registry, sort_dicts=False, width=100) + "\n\n"
    py += 'class EventMetadata(TypedDict):\n    id: str\n    organization_id: str\n    task_id: str\n    sequence: str\n    created_at: str\n\n\n'
    event_types = []
    for kind, definition in registry["types"].items():
        name = ''.join(part.title() for part in kind.replace('.', '_').split('_'))
        required, optional = [], []
        for field_name, field in definition['fields'].items():
            field_type = {'integer': 'int', 'boolean': 'bool'}.get(field['kind'], 'str')
            if 'const' in field:
                field_type = f'Literal[{field["const"]!r}]'
            elif 'enum' in field:
                field_type = 'Literal[' + ', '.join(repr(value) for value in field['enum']) + ']'
            (required if field['required'] else optional).append(f'    {field_name}: {field_type}\n')
        py += f'class _{name}Required(TypedDict):\n' + (''.join(required) or '    pass\n') + '\n\n'
        py += f'class {name}Payload(_{name}Required, total=False):\n' + (''.join(optional) or '    pass\n') + '\n\n'
        py += f'class {name}Event(EventMetadata):\n    type: Literal[{kind!r}]\n    payload_version: Literal[1]\n    data: {name}Payload\n\n\n'
        event_types.append(name+'Event')
    py += 'TypedEvent = ' + ' | '.join(event_types) + '\n'
    return {ROOT / "packages/typescript-sdk/src/eventPayloads.ts": ts, ROOT / "packages/python-sdk/src/agent_platform/event_payloads.py": py}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    for path, content in generated().items():
        if args.check:
            assert path.read_text() == content, f"Stale public protocol declarations: {path.relative_to(ROOT)}"
        else:
            path.write_text(content)
