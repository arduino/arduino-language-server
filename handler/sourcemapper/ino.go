package sourcemapper

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"github.com/sourcegraph/go-lsp"
)

// InoMapper is a mapping between the .ino sketch and the preprocessed .cpp file
type InoMapper struct {
	toCpp map[int]int
	toIno map[int]int
}

// InoToCppLine converts a source (.ino) line into a target (.cpp) line
func (s *InoMapper) InoToCppLine(sourceLine int) int {
	return s.toCpp[sourceLine]
}

// InoToCppRange converts a source (.ino) lsp.Range into a target (.cpp) lsp.Range
func (s *InoMapper) InoToCppRange(r lsp.Range) lsp.Range {
	r.Start.Line = s.InoToCppLine(r.Start.Line)
	r.End.Line = s.InoToCppLine(r.End.Line)
	return r
}

// CppToInoLine converts a target (.cpp) line into a source (.ino) line
func (s *InoMapper) CppToInoLine(targetLine int) int {
	return s.toIno[targetLine]
}

// CppToInoRange converts a target (.cpp) lsp.Range into a source (.ino) lsp.Range
func (s *InoMapper) CppToInoRange(r lsp.Range) lsp.Range {
	r.Start.Line = s.CppToInoLine(r.Start.Line)
	r.End.Line = s.CppToInoLine(r.End.Line)
	return r
}

// CppToInoLineOk converts a target (.cpp) line into a source (.ino) line and
// returns true if the conversion is successful
func (s *InoMapper) CppToInoLineOk(targetLine int) (int, bool) {
	res, ok := s.toIno[targetLine]
	return res, ok
}

// CreateInoMapper create a InoMapper from the given target file
func CreateInoMapper(targetFile io.Reader) *InoMapper {
	sourceLine := -1
	targetLine := 0
	sourceLineMap := make(map[int]int)
	targetLineMap := make(map[int]int)
	scanner := bufio.NewScanner(targetFile)
	for scanner.Scan() {
		lineStr := scanner.Text()
		if strings.HasPrefix(lineStr, "#line") {
			nrEnd := strings.Index(lineStr[6:], " ")
			var l int
			var err error
			if nrEnd > 0 {
				l, err = strconv.Atoi(lineStr[6 : nrEnd+6])
			} else {
				l, err = strconv.Atoi(lineStr[6:])
			}
			if err == nil && l > 0 {
				sourceLine = l - 1
			}
		} else if sourceLine >= 0 {
			sourceLineMap[targetLine] = sourceLine
			targetLineMap[sourceLine] = targetLine
			sourceLine++
		}
		targetLine++
	}
	sourceLineMap[targetLine] = sourceLine
	targetLineMap[sourceLine] = targetLine
	return &InoMapper{
		toIno: sourceLineMap,
		toCpp: targetLineMap,
	}
}

// Update performs an update to the SourceMap considering the deleted lines, the
// insertion line and the inserted text
func (s *InoMapper) Update(deletedLines, insertLine int, insertText string) {
	for i := 1; i <= deletedLines; i++ {
		sourceLine := insertLine + 1
		targetLine := s.toCpp[sourceLine]

		// Shift up all following lines by one and put them into a new map
		newMappings := make(map[int]int)
		maxSourceLine, maxTargetLine := 0, 0
		for t, s := range s.toIno {
			if t > targetLine && s > sourceLine {
				newMappings[t-1] = s - 1
			} else if s > sourceLine {
				newMappings[t] = s - 1
			} else if t > targetLine {
				newMappings[t-1] = s
			}
			if s > maxSourceLine {
				maxSourceLine = s
			}
			if t > maxTargetLine {
				maxTargetLine = t
			}
		}

		// Remove mappings for the deleted line
		delete(s.toIno, maxTargetLine)
		delete(s.toCpp, maxSourceLine)

		// Copy the mappings from the intermediate map
		copyMappings(s.toIno, s.toCpp, newMappings)
	}

	addedLines := strings.Count(insertText, "\n")
	if addedLines > 0 {
		targetLine := s.toCpp[insertLine]

		// Shift down all following lines and put them into a new map
		newMappings := make(map[int]int)
		for t, s := range s.toIno {
			if t > targetLine && s > insertLine {
				newMappings[t+addedLines] = s + addedLines
			} else if s > insertLine {
				newMappings[t] = s + addedLines
			} else if t > targetLine {
				newMappings[t+addedLines] = s
			}
		}

		// Add mappings for the added lines
		for i := 1; i <= addedLines; i++ {
			s.toIno[targetLine+i] = insertLine + i
			s.toCpp[insertLine+i] = targetLine + i
		}

		// Copy the mappings from the intermediate map
		copyMappings(s.toIno, s.toCpp, newMappings)
	}
}

func copyMappings(sourceLineMap, targetLineMap, newMappings map[int]int) {
	for t, s := range newMappings {
		sourceLineMap[t] = s
		targetLineMap[s] = t
	}
	for t, s := range newMappings {
		// In case multiple target lines are present for a source line, use the last one
		if t > targetLineMap[s] {
			targetLineMap[s] = t
		}
	}
}
