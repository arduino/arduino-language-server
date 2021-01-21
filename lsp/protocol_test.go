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
	x := &ProgressParams{
		Token: "token",
		Value: Raw(WorkDoneProgressBegin{
			Title: "some work",
		}),
	}
	data, err := json.Marshal(&x)
	require.NoError(t, err)
	require.JSONEq(t, `{"token":"token", "value":{"kind":"begin","title":"some work"}}`, string(data))

	var begin WorkDoneProgressBegin
	err = json.Unmarshal([]byte(`{"kind":"begin","title":"some work"}`), &begin)
	require.NoError(t, err)

	var report WorkDoneProgressReport
	err = json.Unmarshal([]byte(`{"kind":"report","message":"28/29","percentage":96.551724137931032}`), &report)
	require.NoError(t, err)

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
	err = json.Unmarshal([]byte(msg), &init)
	require.NoError(t, err)
}
