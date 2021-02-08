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

func TestVariousMessages(t *testing.T) {
	t.Run("ProgressParamsMarshalUnmarshal", func(t *testing.T) {
		x := &ProgressParams{
			Token: "token",
			Value: Raw(WorkDoneProgressBegin{
				Title: "some work",
			}),
		}
		data, err := json.Marshal(&x)
		require.NoError(t, err)
		require.JSONEq(t, `{"token":"token", "value":{"kind":"begin","title":"some work"}}`, string(data))
	})

	t.Run("WorkDoneProgressBegin", func(t *testing.T) {
		var begin WorkDoneProgressBegin
		err := json.Unmarshal([]byte(`{"kind":"begin","title":"some work"}`), &begin)
		require.NoError(t, err)
	})

	t.Run("WorkDoneProgressReport", func(t *testing.T) {
		var report WorkDoneProgressReport
		err := json.Unmarshal([]byte(`{"kind":"report","message":"28/29","percentage":96.551724137931032}`), &report)
		require.NoError(t, err)
	})

	t.Run("InitializeResult", func(t *testing.T) {
		msg := `{
		"capabilities":{
			"codeActionProvider":{
				"codeActionKinds":["quickfix","refactor","info"]},
			"completionProvider":{
				"allCommitCharacters":[" ","\t","(",")","[","]","{","}","<",">",":",";",",","+","-","/","*","%","^","&","#","?",".","=","\"","'","|"],
				"resolveProvider":false,
				"triggerCharacters":[".","<",">",":","\"","/"]},
			"declarationProvider":true,
			"definitionProvider":true,
			"documentFormattingProvider":true,
			"documentHighlightProvider":true,
			"documentLinkProvider":{"resolveProvider":false},
			"documentOnTypeFormattingProvider":{
				"firstTriggerCharacter":"\n",
				"moreTriggerCharacter":[]},
			"documentRangeFormattingProvider":true,
			"documentSymbolProvider":true,
			"executeCommandProvider":{"commands":["clangd.applyFix","clangd.applyTweak"]},
			"hoverProvider":true,
			"referencesProvider":true,
			"renameProvider":{"prepareProvider":true},
			"selectionRangeProvider":true,
			"semanticTokensProvider":{
				"full":{"delta":true},
				"legend":{
					"tokenModifiers":[],
					"tokenTypes":["variable","variable","parameter","function","member","function","member","variable","class","enum","enumConstant","type","dependent","dependent","namespace","typeParameter","concept","type","macro","comment"]
				},
				"range":false},
			"signatureHelpProvider":{"triggerCharacters":["(",","]},
			"textDocumentSync":{
				"change":2,
				"openClose":true,
				"save":true
			},
			"typeHierarchyProvider":true,
			"workspaceSymbolProvider":true
		},
		"serverInfo":{"name":"clangd","version":"clangd version 11.0.0 (https://github.com/llvm/llvm-project 176249bd6732a8044d457092ed932768724a6f06)"}}`
		var init InitializeResult
		err := json.Unmarshal([]byte(msg), &init)
		require.NoError(t, err)
	})

	t.Run("DocumentSymbol", func(t *testing.T) {
		msg := `[{"kind":12,"name":"setup","range":{"end":{"character":12,"line":5},"start":{"character":0,"line":5}},"selectionRange":
			{"end":{"character":10,"line":5},"start":{"character":5,"line":5}}},{"kind":12,"name":"newfunc","range":{"end":{"character":14,"line":7},
			"start":{"character":0,"line":7}},"selectionRange":{"end":{"character":12,"line":7},"start":{"character":5,"line":7}}},{"kind":12,"name":
			"altro","range":{"end":{"character":12,"line":9},"start":{"character":0,"line":9}},"selectionRange":{"end":{"character":10,"line":9},"start":
			{"character":5,"line":9}}},{"kind":12,"name":"ancora","range":{"end":{"character":18,"line":11},"start":{"character":0,"line":11}},
			"selectionRange":{"end":{"character":11,"line":11},"start":{"character":5,"line":11}}},{"kind":12,"name":"loop","range":{"end":{
			"character":11,"line":13},"start":{"character":0,"line":13}},"selectionRange":{"end":{"character":9,"line":13},"start":{"character":5,
			"line":13}}},{"kind":12,"name":"secondFunction","range":{"end":{"character":21,"line":15},"start":{"character":0,"line":15}},
			"selectionRange":{"end":{"character":19,"line":15},"start":{"character":5,"line":15}}},{"kind":12,"name":"setup","range":{"end":{
			"character":1,"line":34},"start":{"character":0,"line":17}},"selectionRange":{"end":{"character":10,"line":17},"start":{"character":5,
			"line":17}}},{"kind":12,"name":"newfunc","range":{"end":{"character":1,"line":40},"start":{"character":0,"line":36}},"selectionRange":
			{"end":{"character":12,"line":36},"start":{"character":5,"line":36}}},{"kind":12,"name":"altro","range":{"end":{"character":38,"line":42},
			"start":{"character":0,"line":42}},"selectionRange":{"end":{"character":10,"line":42},"start":{"character":5,"line":42}}},{"kind":12,
			"name":"ancora","range":{"end":{"character":21,"line":47},"start":{"character":0,"line":47}},"selectionRange":{"end":{"character":11,
			"line":47},"start":{"character":5,"line":47}}},{"kind":12,"name":"loop","range":{"end":{"character":24,"line":49},"start":{"character":0,
			"line":49}},"selectionRange":{"end":{"character":9,"line":49},"start":{"character":5,"line":49}}},{"kind":12,"name":"secondFunction",
			"range":{"end":{"character":38,"line":53},"start":{"character":0,"line":53}},"selectionRange":{"end":{"character":19,"line":53},"start":
			{"character":5,"line":53}}}]`
		var symbol DocumentSymbolArrayOrSymbolInformationArray
		err := json.Unmarshal([]byte(msg), &symbol)
		require.NoError(t, err)
	})

	t.Run("EmptyDocumentSymbolMarshalUnmarshal", func(t *testing.T) {
		var symbol DocumentSymbolArrayOrSymbolInformationArray
		err := json.Unmarshal([]byte(`[]`), &symbol)
		require.NoError(t, err)
		data, err := json.Marshal(symbol)
		require.Equal(t, "[]", string(data))
		data, err = json.Marshal(&symbol)
		require.Equal(t, "[]", string(data))
	})
}
