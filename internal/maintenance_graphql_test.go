package internal

import (
	"testing"

	"github.com/graphql-go/graphql/language/parser"
	"github.com/stretchr/testify/require"
)

// The client accepts operation text rather than requiring a named generated
// operation. Its parser -> visitor -> printer path must retain valid anonymous
// operations when variables force the long query syntax.
func TestMaintenanceGraphQLAnonymousBatchRoundTrip(t *testing.T) {
	qb := newQueryBuilder()
	id, field, err := qb.add(`query ($id: Int!, $include: Boolean!) {
  book(id: $id) { id title @include(if: $include) }
}`, map[string]any{"id": 17, "include": true})
	require.NoError(t, err)
	require.Equal(t, "book", field)
	query, vars, err := qb.build()
	require.NoError(t, err)
	_, err = parser.Parse(parser.ParseParams{Source: query})
	require.NoError(t, err, "printed query must be parseable: %s", query)
	require.Equal(t, map[string]any{id + "_id": 17, id + "_include": true}, vars)
	require.Contains(t, query, id+": book(id: $"+id+"_id)")
	require.Contains(t, query, "@include(if: $"+id+"_include)")
	require.NotContains(t, query, "<nil>")
}

func TestMaintenanceGraphQLBatchFragmentAndVariableIsolation(t *testing.T) {
	qb := newQueryBuilder()
	query := `query MaintenanceBook($id: Int!, $include: Boolean!) {
  book(id: $id) { ...MaintenanceBookFields title @include(if: $include) }
}
fragment MaintenanceBookFields on Book { id }`
	first, _, err := qb.add(query, map[string]any{"id": 17, "include": true})
	require.NoError(t, err)
	second, _, err := qb.add(query, map[string]any{"id": 23, "include": false})
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	built, vars, err := qb.build()
	require.NoError(t, err)
	doc, err := parser.Parse(parser.ParseParams{Source: built})
	require.NoError(t, err, "printed batch must be parseable: %s", built)
	require.Len(t, doc.Definitions, 2, "one operation and one deduplicated fragment")
	require.Equal(t, map[string]any{
		first + "_id": 17, first + "_include": true,
		second + "_id": 23, second + "_include": false,
	}, vars)
	for _, id := range []string{first, second} {
		require.Contains(t, built, id+": book(id: $"+id+"_id)")
		require.Contains(t, built, "@include(if: $"+id+"_include)")
	}
}

func TestMaintenanceGraphQLRejectsMalformedOperation(t *testing.T) {
	qb := newQueryBuilder()
	_, _, err := qb.add(`query Broken($id: Int!) { book(id: $id) {`, map[string]any{"id": 17})
	require.ErrorContains(t, err, "failed to parse query")
}
