# License boundaries

- Default license for original server, web, command-line implementation and repository documentation: Elastic License 2.0, full text in ../LICENSE.
- Client SDKs in packages/python-sdk and packages/typescript-sdk: Apache License 2.0, full texts inside each package.
- Examples in examples/python and examples/typescript: Apache License 2.0, full texts inside each examples directory.
- Dependencies retain their upstream licenses. The alpha distribution includes a dependency inventory and license notices; a standard SBOM is deferred and is not claimed. Installing a dependency does not relicense it.
- Client SDKs contain original protocol bindings, never copied server implementations. The schema generator belongs to the server implementation.
- Generated public protocol declarations `contracts/event-payloads.json` and shared examples `contracts/event-conformance.json` use Apache-2.0. SDKs may include these declarations and examples; this exception does not relicense the Go generator or server implementation. Full text for these two protocol artifacts: [PROTOCOL-LICENSE](../contracts/PROTOCOL-LICENSE).
- Additional hosted-service/OEM commercial rights for cloud providers require a separate agreement from the rights holder; the repository does not grant that exception by implication. No agreement has been executed by this implementation work.

The license split was explicitly approved by the product owner. This document does not claim legal review or completed release licensing checks.
