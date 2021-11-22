package ls

import (
	"fmt"

	"github.com/arduino/arduino-language-server/sourcemapper"
	"go.bug.st/lsp"
	"go.bug.st/lsp/jsonrpc"
)

func (ls *INOLanguageServer) clang2IdeRangeAndDocumentURI(logger jsonrpc.FunctionLogger, clangURI lsp.DocumentURI, clangRange lsp.Range) (lsp.DocumentURI, lsp.Range, error) {
	// Sketchbook/Sketch/Sketch.ino      <-> build-path/sketch/Sketch.ino.cpp
	// Sketchbook/Sketch/AnotherTab.ino  <-> build-path/sketch/Sketch.ino.cpp  (different section from above)
	if ls.clangURIRefersToIno(clangURI) {
		// We are converting from preprocessed sketch.ino.cpp back to a sketch.ino file
		idePath, ideRange, err := ls.sketchMapper.CppToInoRangeOk(clangRange)
		if err == nil {
			if ls.sketchMapper.IsPreprocessedCppLine(clangRange.Start.Line) {
				idePath = sourcemapper.NotIno.File
				logger.Logf("Range is in PREPROCESSED section of the sketch")
			}
		} else if _, ok := err.(sourcemapper.AdjustedRangeErr); ok {
			logger.Logf("Range has been END LINE ADJSUTED")
			err = nil
		} else {
			logger.Logf("Range conversion ERROR: %s", err)
			ls.sketchMapper.DebugLogAll()
			return lsp.NilURI, lsp.NilRange, err
		}
		ideURI, err := ls.idePathToIdeURI(logger, idePath)
		logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
		return ideURI, ideRange, err
	}

	// /another/global/path/to/source.cpp <-> /another/global/path/to/source.cpp (same range)
	ideRange := clangRange
	clangPath := clangURI.AsPath()
	inside, err := clangPath.IsInsideDir(ls.buildSketchRoot)
	if err != nil {
		logger.Logf("ERROR: could not determine if '%s' is inside '%s'", clangURI, ls.buildSketchRoot)
		return lsp.NilURI, lsp.NilRange, err
	}
	if !inside {
		ideURI := clangURI
		logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
		return clangURI, clangRange, nil
	}

	// Sketchbook/Sketch/AnotherFile.cpp <-> build-path/sketch/AnotherFile.cpp (same range)
	rel, err := ls.buildSketchRoot.RelTo(clangPath)
	if err != nil {
		logger.Logf("ERROR: could not transform '%s' into a relative path on '%s': %s", clangURI, ls.buildSketchRoot, err)
		return lsp.NilURI, lsp.NilRange, err
	}
	idePath := ls.sketchRoot.JoinPath(rel).String()
	ideURI, err := ls.idePathToIdeURI(logger, idePath)
	logger.Logf("Range: %s:%s -> %s:%s", clangURI, clangRange, ideURI, ideRange)
	return ideURI, clangRange, err
}

func (ls *INOLanguageServer) clang2IdeDocumentHighlight(logger jsonrpc.FunctionLogger, clangHighlight lsp.DocumentHighlight, cppURI lsp.DocumentURI) (lsp.DocumentHighlight, error) {
	_, ideRange, err := ls.clang2IdeRangeAndDocumentURI(logger, cppURI, clangHighlight.Range)
	if err != nil {
		return lsp.DocumentHighlight{}, err
	}
	return lsp.DocumentHighlight{
		Kind:  clangHighlight.Kind,
		Range: ideRange,
	}, nil
}

func (ls *INOLanguageServer) clang2IdeDiagnostics(logger jsonrpc.FunctionLogger, clangDiagsParams *lsp.PublishDiagnosticsParams) ([]*lsp.PublishDiagnosticsParams, error) {
	clangURI := clangDiagsParams.URI
	if !ls.clangURIRefersToIno(clangURI) {
		ideDiags := []lsp.Diagnostic{}
		ideDiagsURI := lsp.DocumentURI{}
		for _, clangDiag := range clangDiagsParams.Diagnostics {
			ideURI, ideRange, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangDiag.Range)
			if err != nil {
				return nil, err
			}
			if ideURI.String() == sourcemapper.NotInoURI.String() {
				continue
			}
			if ideDiagsURI.String() == "" {
				ideDiagsURI = ideURI
			} else if ideDiagsURI.String() != ideURI.String() {
				return nil, fmt.Errorf("unexpected URI %s: it should be %s", ideURI, ideURI)
			}
			ideDiag := clangDiag
			ideDiag.Range = ideRange
			ideDiags = append(ideDiags, ideDiag)
		}
		return []*lsp.PublishDiagnosticsParams{
			{
				URI:         ideDiagsURI,
				Diagnostics: ideDiags,
			},
		}, nil
	}

	// Diagnostics coming from sketch.ino.cpp refers to all .ino files, so it must update
	// the diagnostics list of all .ino files altogether.
	// XXX: maybe this logic can be moved outside of this conversion function, make it much
	// more straighforward.
	allIdeInoDiagsParams := map[lsp.DocumentURI]*lsp.PublishDiagnosticsParams{}
	for ideInoURI := range ls.ideInoDocsWithDiagnostics {
		allIdeInoDiagsParams[ideInoURI] = &lsp.PublishDiagnosticsParams{
			URI:         ideInoURI,
			Diagnostics: []lsp.Diagnostic{},
		}
	}
	ls.ideInoDocsWithDiagnostics = map[lsp.DocumentURI]bool{}

	for _, clangDiag := range clangDiagsParams.Diagnostics {
		ideURI, ideRange, err := ls.clang2IdeRangeAndDocumentURI(logger, clangURI, clangDiag.Range)
		if err != nil {
			return nil, err
		}
		if ideURI.String() == sourcemapper.NotInoURI.String() {
			continue
		}

		ideInoDiagsParams, ok := allIdeInoDiagsParams[ideURI]
		if !ok {
			ideInoDiagsParams = &lsp.PublishDiagnosticsParams{
				URI:         ideURI,
				Diagnostics: []lsp.Diagnostic{},
			}
			allIdeInoDiagsParams[ideURI] = ideInoDiagsParams
		}

		ideInoDiag := clangDiag
		ideInoDiag.Range = ideRange
		ideInoDiagsParams.Diagnostics = append(ideInoDiagsParams.Diagnostics, ideInoDiag)

		ls.ideInoDocsWithDiagnostics[ideURI] = true
	}

	ideInoDiagParams := []*lsp.PublishDiagnosticsParams{}
	for _, v := range allIdeInoDiagsParams {
		ideInoDiagParams = append(ideInoDiagParams, v)
	}
	return ideInoDiagParams, nil
}

func (ls *INOLanguageServer) clang2IdeSymbolInformation(clangSymbolsInformation []lsp.SymbolInformation) []lsp.SymbolInformation {
	panic("not implemented")
}
