// This file is part of arduino-language-server.
//
// Copyright 2022 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU Affero General Public License version 3,
// which covers the main part of arduino-language-server.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/agpl-3.0.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to
// modify or otherwise use the software for commercial activities involving the
// Arduino software without disclosing the source code of your own applications.
// To purchase a commercial license, send an email to license@arduino.cc.

package ls

import (
	"strconv"

	"github.com/arduino/arduino-language-server/sourcemapper"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (ls *INOLanguageServer) clangURIRefersToIno(clangURI lsp.DocumentURI) bool {
	return clangURI.AsPath().EquivalentTo(ls.buildSketchCpp)
}

func (ls *INOLanguageServer) clang2IdeRangeAndDocumentURI(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangRange lsp.Range) (lsp.DocumentURI, lsp.Range, bool, error) {
	return ls.clang2IdeRangeAndDocumentURI2(logger, clangURI, clangRange, nil)
}

// Convert Range and DocumentURI from Clang to IDE.
// Returns:
// - The IDE DocumentURI and Range
// - a boolean that is true if the clang range is in the preprocessed area of the sketch
// - an error
func (ls *INOLanguageServer) clang2IdeRangeAndDocumentURI2(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangRange lsp.Range, opts *TranslationOpts) (lsp.DocumentURI, lsp.Range, bool, error) {
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
		logger.Logf("Range: %s:%s -> %s:%s (.ino)", clangURI, clangRange, ideURI, ideRange)
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
		logger.Logf("Range: %s:%s -> %s:%s (ext file)", clangURI, clangRange, ideURI, ideRange)
		return clangURI, clangRange, false, nil
	}

	// Sketchbook/Sketch/AnotherFile.cpp <-> build-path/sketch/AnotherFile.cpp (one line offset)
	rel, err := ls.buildSketchRoot.RelTo(clangPath)
	if err != nil {
		logger.Logf("ERROR: could not transform '%s' into a relative path on '%s': %s", clangURI, ls.buildSketchRoot, err)
		return lsp.NilURI, lsp.NilRange, false, err
	}
	idePath := ls.sketchRoot.JoinPath(rel).String()

	var ideURI lsp.DocumentURI
	if opts == nil || !opts.loadRelToIde {
		ideURI, err = ls.idePathToIdeURI(logger, idePath)
	} else {
		doc, ok := ls.trackedIdeDocs[idePath]
		if !ok {
			var shouldOpen bool
			doc, shouldOpen, err = makeTextDocumentItem(logger, idePath)
			if err != nil {
				logger.Logf("ERROR: could not open '%s': %s", idePath, err)
			}
			if shouldOpen {
				ls.implTextDocumentDidOpenNotifFromIDE(logger, doc)
			}

		}
		ideURI = doc.URI
	}
	if ideRange.End.Line > 0 {
		ideRange.End.Line--
	}
	if ideRange.Start.Line > 0 {
		ideRange.Start.Line--
	}
	logger.Logf("Range: %s:%s -> %s:%s (.cpp/.h)", clangURI, clangRange, ideURI, ideRange)
	return ideURI, ideRange, false, err
}

func (ls *INOLanguageServer) clang2IdeDocumentURI(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI) (lsp.DocumentURI, error) {
	// Sketchbook/Sketch/Sketch.ino      <-> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <-> build-path/sketch/Sketch.ino.cpp  (different section from above)
	if ls.clangURIRefersToIno(clangURI) {
		// the URI may refer to any .ino, without a range reference pick the first tracked .ino
		for _, ideDoc := range ls.trackedIdeDocs {
			if ideDoc.URI.Ext() == ".ino" {
				logger.Logf("%s -> %s", clangURI, ideDoc.URI)
				return ideDoc.URI, nil
			}
		}
		return lsp.DocumentURI{}, &UnknownURIError{URI: clangURI}
	}

	// /another/global/path/to/source.cpp <-> /another/global/path/to/source.cpp
	clangPath := clangURI.AsPath()
	inside, err := clangPath.IsInsideDir(ls.buildSketchRoot)
	if err != nil {
		logger.Logf("ERROR: could not determine if '%s' is inside '%s'", clangURI, ls.buildSketchRoot)
		return lsp.DocumentURI{}, err
	}
	if !inside {
		ideURI := clangURI
		logger.Logf("%s -> %s", clangURI, ideURI)
		return ideURI, nil
	}

	// Sketchbook/Sketch/AnotherFile.cpp <-> build-path/sketch/AnotherFile.cpp
	rel, err := ls.buildSketchRoot.RelTo(clangPath)
	if err != nil {
		logger.Logf("ERROR: could not transform '%s' into a relative path on '%s': %s", clangURI, ls.buildSketchRoot, err)
		return lsp.DocumentURI{}, err
	}
	idePath := ls.sketchRoot.JoinPath(rel).String()
	ideURI, err := ls.idePathToIdeURI(logger, idePath)
	logger.Logf("%s -> %s", clangURI, ideURI)
	return ideURI, err
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

	// Convert empty diagnostic directly (otherwise they will be missed from the next loop)
	if len(clangDiagsParams.Diagnostics) == 0 {
		ideURI, err := ls.clang2IdeDocumentURI(logger, clangDiagsParams.URI)
		if err != nil {
			return nil, err
		}
		allIdeDiagsParams[ideURI] = &lsp.PublishDiagnosticsParams{
			URI:         ideURI,
			Version:     clangDiagsParams.Version,
			Diagnostics: []lsp.Diagnostic{},
		}
		return allIdeDiagsParams, nil
	}

	// Collect all diagnostics into different sets
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

func (ls *INOLanguageServer) clang2IdeDocumentSymbols(logger jsonrpc.FunctionLogger, clangSymbols []lsp.DocumentSymbol, clangURI lsp.DocumentURI, origIdeURI lsp.DocumentURI) ([]lsp.DocumentSymbol, error) {
	logger.Logf("%s (%d document symbols)", clangURI, len(clangSymbols))

	ideSymbols := []lsp.DocumentSymbol{}
	for _, clangSymbol := range clangSymbols {
		logger.Logf("  > convert %s %s", clangSymbol.Kind, clangSymbol.Range)
		ideURI, ideRange, isPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangSymbol.Range)
		if err != nil {
			logger.Logf("    filtering out invalid symbol range: %s", err)
			continue
		}
		if isPreprocessed {
			logger.Logf("    symbol is in the preprocessed section of the sketch.ino.cpp, skipping")
			continue
		}
		if ideURI != origIdeURI {
			logger.Logf("    filtering out symbol related to %s", ideURI)
			continue
		}
		ideSelectionURI, ideSelectionRange, isSelectionPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangSymbol.SelectionRange)
		if err != nil {
			logger.Logf("    filtering out invalid symbol selection-range: %s", err)
			continue
		}
		if ideSelectionURI != ideURI || isSelectionPreprocessed {
			logger.Logf("    ERROR: doc of symbol-selection-range does not match doc of symbol-range")
			logger.Logf("        range     %s > %s:%s", clangSymbol.Range, ideURI, ideRange)
			logger.Logf("        selection %s > %s:%s", clangSymbol.SelectionRange, ideSelectionURI, ideSelectionRange)
			continue
		}

		ideChildren, err := ls.clang2IdeDocumentSymbols(logger, clangSymbol.Children, clangURI, origIdeURI)
		if err != nil {
			logger.Logf("    filtering out invalid document-symbol: %s", err)
			continue
		}

		ideSymbols = append(ideSymbols, lsp.DocumentSymbol{
			Name:           clangSymbol.Name,
			Detail:         clangSymbol.Detail,
			Deprecated:     clangSymbol.Deprecated,
			Kind:           clangSymbol.Kind,
			Range:          ideRange,
			SelectionRange: ideSelectionRange,
			Children:       ideChildren,
			Tags:           ls.clang2IdeSymbolTags(logger, clangSymbol.Tags),
		})
	}

	return ideSymbols, nil
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
	return ls.clang2IdeLocationsArray2(logger, clangLocations, nil)
}
func (ls *INOLanguageServer) clang2IdeLocationsArray2(logger jsonrpc.FunctionLogger, clangLocations []lsp.Location, opts *TranslationOpts) ([]lsp.Location, error) {
	ideLocations := []lsp.Location{}
	for _, clangLocation := range clangLocations {
		ideLocation, inPreprocessed, err := ls.clang2IdeLocation2(logger, clangLocation, opts)
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
	return ls.clang2IdeLocation2(logger, clangLocation, nil)
}
func (ls *INOLanguageServer) clang2IdeLocation2(logger jsonrpc.FunctionLogger, clangLocation lsp.Location, opts *TranslationOpts) (lsp.Location, bool, error) {
	ideURI, ideRange, inPreprocessed, err := ls.clang2IdeRangeAndDocumentURI2(logger, clangLocation.URI, clangLocation.Range, opts)
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

func (ls *INOLanguageServer) clang2IdeWorkspaceEdit(logger jsonrpc.FunctionLogger, clangWorkspaceEdit *lsp.WorkspaceEdit) (*lsp.WorkspaceEdit, error) {
	ideChanges := map[lsp.DocumentURI][]lsp.TextEdit{}
	for clangURI, clangChanges := range clangWorkspaceEdit.Changes {
		for _, clangTextEdit := range clangChanges {
			ideURI, ideTextEdit, isPreprocessed, err := ls.clang2IdeTextEdit(logger, clangURI, clangTextEdit)
			if isPreprocessed {
				logger.Logf("- ignore edit in preprocessed area")
				continue
			}
			if err != nil {
				return nil, err
			}
			ideChanges[ideURI] = append(ideChanges[ideURI], ideTextEdit)
		}
	}
	ideWorkspaceEdit := &lsp.WorkspaceEdit{
		Changes:           ideChanges,
		ChangeAnnotations: clangWorkspaceEdit.ChangeAnnotations,
	}
	return ideWorkspaceEdit, nil
}

func (ls *INOLanguageServer) clang2IdeTextEdit(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangTextEdit lsp.TextEdit) (lsp.DocumentURI, lsp.TextEdit, bool, error) {
	ideURI, ideRange, isPreprocessed, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangTextEdit.Range)
	ideTextEdit := lsp.TextEdit{
		NewText: clangTextEdit.NewText,
		Range:   ideRange,
	}
	return ideURI, ideTextEdit, isPreprocessed, err
}
