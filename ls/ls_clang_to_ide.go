package ls

import (
	"strconv"

	"github.com/arduino/arduino-language-server/sourcemapper"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (ls *INOLanguageServer) clang2IdeRangeAndDocumentURI(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangRange lsp.Range) (lsp.DocumentURI, lsp.Range, bool, error) {
	// Sketchbook/Sketch/Sketch.ino      <-> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <-> build-path/sketch/Sketch.ino.cpp  (different section from above)
	if ls.clangURIRefersToIno(clangURI) {
		// We are converting from preprocessed sketch.ino.cpp back to a sketch.ino file
		idePath, ideRange, err := ls.sketchMapper.CppToInoRangeOk(clangRange)
		if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			logger.Logf("Range has been END LINE ADJSUTED")
		} else if err != nil {
			logger.Logf("Range conversion ERROR: %s", err)
			ls.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, false, err
		}
		ideURI, err := ls.idePathToIdeURI(logger, idePath)
		if err != nil {
			logger.Logf("Range conversion ERROR: %s", err)
			ls.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, false, err
		}
		inPreprocessed := ls.sketchMapper.IsPreprocessedCppLine(clangRange.Start.Line)
		if inPreprocessed {
			logger.Logf("Range is in PREPROCESSED section of the sketch")
		}
		logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
		return ideURI, ideRange, inPreprocessed, err
	}

	// /another/global/path/to/source.cpp <-> /another/global/path/to/source.cpp (same range)
	ideRange := clangRange
	clangPath := clangURI.AsPath()
	inside, err := clangPath.IsInsideDir(ls.buildSketchRoot)
	if err != nil {
		logger.Logf("ERROR: could not determine if '%s' is inside '%s'", clangURI, ls.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, false, err
	}
	if !inside {
		ideURI := clangURI
		logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
		return clangURI, clangRange, false, nil
	}

	// Sketchbook/Sketch/AnotherFile.cpp <-> build-path/sketch/AnotherFile.cpp (same range)
	rel, err := ls.buildSketchRoot.RelTo(clangPath)
	if err != nil {
		logger.Logf("ERROR: could not transform '%s' into a relative path on '%s': %s", clangURI, ls.buildSketchRoot, err)
		return lsp.NilURI, lsp.NilRange, false, err
	}
	idePath := ls.sketchRoot.JoinPath(rel).String()
	ideURI, err := ls.idePathToIdeURI(logger, idePath)
	logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
	return ideURI, clangRange, false, err
}

func (ls *INOLanguageServer) clang2IdeDocumentHighlight(logger jsonrpc.FunctionLogger, clangHighlight lsp.DocumentHighlight, cppURI lsp.DocumentURI) (lsp.DocumentHighlight, bool, error) {
	_, ideRange, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, cppURI, clangHighlight.Range)
	if err != nil || inPreprocessed {
		return lsp.DocumentHighlight{}, inPreprocessed, err
	}
	return lsp.DocumentHighlight{
		Kind:  clangHighlight.Kind,
		Range: ideRange,
	}, false, nil
}

func (ls *INOLanguageServer) clang2IdeDiagnostics(logger jsonrpc.FunctionLogger, clangDiagsParams *lsp.PublishDiagnosticsParams) (map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams, error) {
	// If diagnostics comes from sketch.ino.cpp they may refer to multiple .ino files,
	// so we collect all of the into a map.
	allIdeDiagsParams := map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams{}

	for _, clangDiagnostic := range clangDiagsParams.Diagnostics {
		ideURI, ideDiagnostic, inPreprocessed, err := ls.clang2IdeDiagnostic(logger, clangDiagsParams.URI, clangDiagnostic)
		if err != nil {
			return nil, err
		}
		if inPreprocessed {
			continue
		}
		if _, ok := allIdeDiagsParams[ideURI]; !ok {
			allIdeDiagsParams[ideURI] = &lsp.PublishDiagnosticsParams{URI: ideURI}
		}
		allIdeDiagsParams[ideURI].Diagnostics = append(allIdeDiagsParams[ideURI].Diagnostics, ideDiagnostic)
	}

	return allIdeDiagsParams, nil
}

func (ls *INOLanguageServer) clang2IdeDiagnostic(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangDiagnostic lsp.Diagnostic) (lsp.DocumentURI, lsp.Diagnostic, bool, error) {
	ideURI, ideRange, inPreproccesed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangDiagnostic.Range)
	if err != nil || inPreproccesed {
		return lsp.DocumentURI{}, lsp.Diagnostic{}, inPreproccesed, err
	}

	ideDiagnostic := clangDiagnostic
	ideDiagnostic.Range = ideRange

	if len(clangDiagnostic.RelatedInformation) > 0 {
		ideInfos, err := ls.clang2IdeDiagnosticRelatedInformationArray(logger, clangDiagnostic.RelatedInformation)
		if err != nil {
			return lsp.DocumentURI{}, lsp.Diagnostic{}, false, err
		}
		ideDiagnostic.RelatedInformation = ideInfos
	}
	return ideURI, ideDiagnostic, false, nil
}

func (ls *INOLanguageServer) clang2IdeDiagnosticRelatedInformationArray(logger jsonrpc.FunctionLogger, clangInfos []lsp.DiagnosticRelatedInformation) ([]lsp.DiagnosticRelatedInformation, error) {
	ideInfos := []lsp.DiagnosticRelatedInformation{}
	for _, clangInfo := range clangInfos {
		ideLocation, inPreprocessed, err := ls.clang2IdeLocation(logger, clangInfo.Location)
		if err != nil {
			return nil, err
		}
		if inPreprocessed {
			logger.Logf("Ignoring in-preprocessed-section diagnostic related information")
			continue
		}
		ideInfos = append(ideInfos, lsp.DiagnosticRelatedInformation{
			Message:  clangInfo.Message,
			Location: ideLocation,
		})
	}
	return ideInfos, nil
}

func (ls *INOLanguageServer) clang2IdeDocumentSymbols(logger jsonrpc.FunctionLogger, clangSymbols []lsp.DocumentSymbol, ideRequestedURI lsp.DocumentURI) []lsp.DocumentSymbol {
	logger.Logf("documentSymbol(%d document symbols)", len(clangSymbols))
	ideRequestedPath := ideRequestedURI.AsPath().String()
	logger.Logf("    filtering for requested ino file: %s", ideRequestedPath)
	if ideRequestedURI.Ext() != ".ino" || len(clangSymbols) == 0 {
		return clangSymbols
	}

	ideSymbols := []lsp.DocumentSymbol{}
	for _, clangSymbol := range clangSymbols {
		logger.Logf("    > convert %s %s", clangSymbol.Kind, clangSymbol.Range)
		if ls.sketchMapper.IsPreprocessedCppLine(clangSymbol.Range.Start.Line) {
			logger.Logf("      symbol is in the preprocessed section of the sketch.ino.cpp")
			continue
		}

		idePath, ideRange := ls.sketchMapper.CppToInoRange(clangSymbol.Range)
		ideSelectionPath, ideSelectionRange := ls.sketchMapper.CppToInoRange(clangSymbol.SelectionRange)

		if idePath != ideSelectionPath {
			logger.Logf("      ERROR: symbol range and selection belongs to different URI!")
			logger.Logf("        symbol %s != selection %s", clangSymbol.Range, clangSymbol.SelectionRange)
			logger.Logf("        %s:%s != %s:%s", idePath, ideRange, ideSelectionPath, ideSelectionRange)
			continue
		}

		if idePath != ideRequestedPath {
			logger.Logf("    skipping symbol related to %s", idePath)
			continue
		}

		ideSymbols = append(ideSymbols, lsp.DocumentSymbol{
			Name:           clangSymbol.Name,
			Detail:         clangSymbol.Detail,
			Deprecated:     clangSymbol.Deprecated,
			Kind:           clangSymbol.Kind,
			Range:          ideRange,
			SelectionRange: ideSelectionRange,
			Children:       ls.clang2IdeDocumentSymbols(logger, clangSymbol.Children, ideRequestedURI),
			Tags:           ls.clang2IdeSymbolTags(logger, clangSymbol.Tags),
		})
	}

	return ideSymbols
}

func (ls *INOLanguageServer) cland2IdeTextEdits(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangTextEdits []lsp.TextEdit) (map[lsp.DocumentURI][]lsp.TextEdit, error) {
	logger.Logf("%s clang/textEdit (%d elements)", clangURI, len(clangTextEdits))
	allIdeTextEdits := map[lsp.DocumentURI][]lsp.TextEdit{}
	for _, clangTextEdit := range clangTextEdits {
		ideURI, ideTextEdit, inPreprocessed, err := ls.cpp2inoTextEdit(logger, clangURI, clangTextEdit)
		if err != nil {
			return nil, err
		}
		logger.Logf("  > %s:%s -> %s", clangURI, clangTextEdit.Range, strconv.Quote(clangTextEdit.NewText))
		if inPreprocessed {
			logger.Logf(("    ignoring in-preprocessed-section edit"))
			continue
		}
		allIdeTextEdits[ideURI] = append(allIdeTextEdits[ideURI], ideTextEdit)
	}

	logger.Logf("converted to:")

	for ideURI, ideTextEdits := range allIdeTextEdits {
		logger.Logf("  %s ino/textEdit (%d elements)", ideURI, len(ideTextEdits))
		for _, ideTextEdit := range ideTextEdits {
			logger.Logf("    > %s:%s -> %s", ideURI, ideTextEdit.Range, strconv.Quote(ideTextEdit.NewText))
		}
	}
	return allIdeTextEdits, nil
}

func (ls *INOLanguageServer) clang2IdeLocationsArray(logger jsonrpc.FunctionLogger, clangLocations []lsp.Location) ([]lsp.Location, error) {
	ideLocations := []lsp.Location{}
	for _, clangLocation := range clangLocations {
		ideLocation, inPreprocessed, err := ls.clang2IdeLocation(logger, clangLocation)
		if err != nil {
			logger.Logf("ERROR converting location %s: %s", clangLocation, err)
			return nil, err
		}
		if inPreprocessed {
			logger.Logf("ignored in-preprocessed-section location")
			continue
		}
		ideLocations = append(ideLocations, ideLocation)
	}
	return ideLocations, nil
}

func (ls *INOLanguageServer) clang2IdeLocation(logger jsonrpc.FunctionLogger, clangLocation lsp.Location) (lsp.Location, bool, error) {
	ideURI, ideRange, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangLocation.URI, clangLocation.Range)
	return lsp.Location{
		URI:   ideURI,
		Range: ideRange,
	}, inPreprocessed, err
}

func (ls *INOLanguageServer) clang2IdeSymbolTags(logger jsonrpc.FunctionLogger, clangSymbolTags []lsp.SymbolTag) []lsp.SymbolTag {
	if len(clangSymbolTags) == 0 || clangSymbolTags == nil {
		return clangSymbolTags
	}
	panic("not implemented")
}

func (ls *INOLanguageServer) clang2IdeSymbolsInformation(logger jsonrpc.FunctionLogger, clangSymbolsInformation []lsp.SymbolInformation) []lsp.SymbolInformation {
	logger.Logf("SymbolInformation (%d elements):", len(clangSymbolsInformation))
	panic("not implemented")
}
