// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

// ToolRoute exposes only static registration metadata for contract generation.
// It describes support, never the active transport mutation allowlist or access.
type ToolRoute struct {
	Name, Method, Path string
	OperatorOptIn      bool
}

func PublicToolRoutes() ([]ToolRoute, error) {
	specs, err := catalog()
	if err != nil {
		return nil, err
	}
	out := make([]ToolRoute, 0, len(specs))
	for _, s := range specs {
		out = append(out, ToolRoute{s.Name, s.Method, s.Path, s.Mutation})
	}
	return out, nil
}
