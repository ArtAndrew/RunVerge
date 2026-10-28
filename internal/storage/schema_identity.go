// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/installation"
	"io/fs"
	"sort"
)

// EmbeddedSchemaIdentity describes the exact immutable migration inputs without
// opening PostgreSQL. It is not a claim that any database has applied them.
func EmbeddedSchemaIdentity() []installation.Migration {
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	result := make([]installation.Migration, 0, len(names))
	for _, name := range names {
		raw, _ := migrations.ReadFile(name)
		result = append(result, installation.Migration{Name: name, SHA256: Hash(raw)})
	}
	return result
}
