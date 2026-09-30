package store

import "strings"

// schema is the Postgres schema holding a product's Mastra memory and the
// SDK's own tables (A-Pen: public, WinDoc: windoc_mastra). Every query names
// it explicitly instead of relying on the connection's search_path.
type schema struct {
	quoted string
}

func newSchema(name string) schema {
	if name == "" {
		panic("ai-agent-sdk store: schema is required (A-Pen: \"public\", WinDoc: \"windoc_mastra\")")
	}
	return schema{quoted: `"` + strings.ReplaceAll(name, `"`, `""`) + `"`}
}

// sql replaces every {schema} in the query with the quoted schema name.
func (s schema) sql(query string) string {
	return strings.ReplaceAll(query, "{schema}", s.quoted)
}
