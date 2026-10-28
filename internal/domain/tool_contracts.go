// SPDX-License-Identifier: Elastic-2.0
package domain

// These select trusted-host result interpretation, not permissions discovered
// from an MCP peer. They do not change the public ToolInfo/API wire shape.
const ToolResultContractActionV1 = "agent_platform_action_v1"
const ToolResultContractMCPReadTextV1 = "mcp_read_text_v1"
