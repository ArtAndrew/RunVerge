# SPDX-License-Identifier: Apache-2.0
"""Inspect by default; explicitly create and statically validate one plan.

This example never approves, applies, verifies, mints credentials or retries a
mutation. Supply an already authorized platform token in the environment.
"""
import argparse
import json
import os
from agent_platform import Client, AgentPlatformError


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', default=os.getenv('AGENT_PLATFORM_URL', 'http://127.0.0.1:8080'))
    parser.add_argument('--create-plan', metavar='DESTINATION_ID')
    parser.add_argument('--destination-version', type=int)
    parser.add_argument('--known-resource-version', type=int)
    parser.add_argument('--grant-id')
    parser.add_argument('--request-key')
    parser.add_argument('--reason')
    args = parser.parse_args()
    if args.create_plan is not None:
        if not args.create_plan.strip():
            parser.error("Destination ID must be nonblank")
        if args.destination_version is None or not 1 <= args.destination_version <= 2147483647 or args.known_resource_version is None or not 0 <= args.known_resource_version <= 2147483647 or not args.reason or not args.reason.strip():
            parser.error('Plan mode requires exact --destination-version, --known-resource-version (0 means absent) and --reason')
        if not args.request_key or len(args.request_key) > 100 or any(ord(c) < 33 or ord(c) > 126 for c in args.request_key):
            parser.error('Plan mode requires a stable --request-key of 1..100 printable ASCII characters')
    elif any(x is not None for x in (args.destination_version, args.known_resource_version, args.grant_id, args.request_key, args.reason)):
        parser.error('Plan options require explicit --create-plan')
    token, org = os.getenv('AGENT_PLATFORM_TOKEN'), os.getenv('AGENT_PLATFORM_ORGANIZATION_ID')
    if not token or not org:
        parser.error('Set AGENT_PLATFORM_TOKEN and AGENT_PLATFORM_ORGANIZATION_ID securely')
    client = Client(args.url, token=token, organization_id=org)
    catalog = client.configuration_catalog()
    destinations = client.configuration_destinations()
    if args.create_plan is None:
        print(json.dumps(dict(catalog=catalog, destinations=destinations.data, next_cursor=destinations.next_cursor), indent=2))
        return 0
    for page in range(20):
        destination = next((d for d in destinations.data if d['id'] == args.create_plan and d['version'] == args.destination_version and d['active']), None)
        if destination or not destinations.next_cursor or page == 19:
            break
        destinations = client.configuration_destinations(cursor=destinations.next_cursor)
    if not destination:
        parser.error('Exact active destination not found in the first 20 pages; inspect with the SDK before planning')
    request = dict(base_revision=catalog['revision'], ttl_seconds=600, operations=[dict(id='setup', destination_id=destination['id'], destination_version=destination['version'], expected_resource_version=args.known_resource_version, active=True)], reason=args.reason)
    if args.grant_id:
        request['grant_id'] = args.grant_id
    plan = client.create_configuration_plan(request, idempotency_key=args.request_key + '-plan')
    validation = client.validate_configuration_plan(plan['id'], dict(expected_version=plan['version'], plan_digest=plan['digest']), idempotency_key=args.request_key + '-validate')
    print(json.dumps(dict(plan_id=plan['id'], version=plan['version'], digest=plan['digest'], expires_at=plan['expires_at'], validation_id=validation['id'], valid=validation['valid'], decision=validation['decision']['outcome'], obligations=validation['decision']['obligations'], reason_codes=validation['decision']['reason_codes'], applied=False, connection_checked=False), indent=2))
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except AgentPlatformError as error:
        raise SystemExit(f'{type(error).__name__}: request stopped; inspect the current record before an explicit retry') from None
