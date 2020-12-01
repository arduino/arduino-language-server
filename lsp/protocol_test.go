package lsp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentSymbolParse(t *testing.T) {
	docin := `
	[
		{
			"kind":12,
			"name":"setup",
			"range": {"end": {"character":11,"line":6},"start": {"character":0,"line":6}},
			"selectionRange":{"end":{"character":10,"line":6},"start":{"character":5,"line":6}}
		},{
			"kind":12,
			"name":"newfunc",
			"range":{"end":{"character":13,"line":8},"start":{"character":0,"line":8}},
			"selectionRange":{"end":{"character":12,"line":8},"start":{"character":5,"line":8}}
		},{
			"kind":12,
			"name":"loop",
			"range":{"end":{"character":10,"line":10},"start":{"character":0,"line":10}},
			"selectionRange":{"end":{"character":9,"line":10},"start":{"character":5,"line":10}}
		},{
			"kind":12,
			"name":"secondFunction",
			"range":{"end":{"character":20,"line":12},"start":{"character":0,"line":12}},
			"selectionRange":{"end":{"character":19,"line":12},"start":{"character":5,"line":12}}
		},{
			"kind":12,
			"name":"setup",
			"range":{"end":{"character":0,"line":21},"start":{"character":0,"line":14}},
			"selectionRange":{"end":{"character":10,"line":14},"start":{"character":5,"line":14}}
		},{
			"kind":12,
			"name":"newfunc",
			"range":{"end":{"character":16,"line":23},"start":{"character":0,"line":23}},
			"selectionRange":{"end":{"character":12,"line":23},"start":{"character":5,"line":23}}
		},{
			"kind":12,
			"name":"loop",
			"range":{"end":{"character":0,"line":26},"start":{"character":0,"line":24}},
			"selectionRange":{"end":{"character":9,"line":24},"start":{"character":5,"line":24}}
		},{
			"kind":12,
			"name":"secondFunction",
			"range":{"end":{"character":0,"line":32},"start":{"character":0,"line":30}},
			"selectionRange":{"end":{"character":19,"line":30},"start":{"character":5,"line":30}}
		}
	]`
	var res DocumentSymbolArrayOrSymbolInformationArray
	err := json.Unmarshal([]byte(docin), &res)
	require.NoError(t, err)
	require.NotNil(t, res.DocumentSymbolArray)
	symbols := *res.DocumentSymbolArray
	require.Equal(t, SymbolKind(12), symbols[2].Kind)
	require.Equal(t, "loop", symbols[2].Name)
	require.Equal(t, "10:0-10:10", symbols[2].Range.String())
	require.Equal(t, "10:5-10:9", symbols[2].SelectionRange.String())
	fmt.Printf("%+v\n", res)
}
