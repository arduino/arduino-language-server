package sourcemapper

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"github.com/bcmi-labs/arduino-language-server/lsp"
)

// InoMapper is a mapping between the .ino sketch and the preprocessed .cpp file
type InoMapper struct {
	InoText map[lsp.DocumentURI]*SourceRevision
	CppText *SourceRevision
	CppFile lsp.DocumentURI
	toCpp   map[InoLine]int // Converts File.ino:line -> line
	toIno   map[int]InoLine // Convers line -> File.ino:line
}

type SourceRevision struct {
	Version int
	Text    string
}

// InoLine is a line number into an .ino file
type InoLine struct {
	File string
	Line int
}

// InoToCppLine converts a source (.ino) line into a target (.cpp) line
func (s *InoMapper) InoToCppLine(sourceURI lsp.DocumentURI, line int) int {
	return s.toCpp[InoLine{sourceURI.Unbox(), line}]
}

// InoToCppLineOk converts a source (.ino) line into a target (.cpp) line
func (s *InoMapper) InoToCppLineOk(sourceURI lsp.DocumentURI, line int) (int, bool) {
	res, ok := s.toCpp[InoLine{sourceURI.Unbox(), line}]
	return res, ok
}

func (s *InoMapper) InoToCppLSPRange(sourceURI lsp.DocumentURI, r lsp.Range) lsp.Range {
	res := r
	res.Start.Line = s.InoToCppLine(sourceURI, r.Start.Line)
	res.End.Line = s.InoToCppLine(sourceURI, r.End.Line)
	return res
}

// CppToInoLine converts a target (.cpp) line into a source.ino:line
func (s *InoMapper) CppToInoLine(targetLine int) (string, int) {
	res := s.toIno[targetLine]
	return res.File, res.Line
}

// CppToInoRange converts a target (.cpp) lsp.Range into a source.ino:lsp.Range
func (s *InoMapper) CppToInoRange(r lsp.Range) (string, lsp.Range) {
	startFile, startLine := s.CppToInoLine(r.Start.Line)
	endFile, endLine := s.CppToInoLine(r.End.Line)
	res := r
	res.Start.Line = startLine
	res.End.Line = endLine
	if startFile != endFile {
		panic("invalid range conversion")
	}
	return startFile, res
}

// CppToInoLineOk converts a target (.cpp) line into a source (.ino) line and
// returns true if the conversion is successful
func (s *InoMapper) CppToInoLineOk(targetLine int) (string, int, bool) {
	res, ok := s.toIno[targetLine]
	return res.File, res.Line, ok
}

// CreateInoMapper create a InoMapper from the given target file
func CreateInoMapper(targetFile io.Reader) *InoMapper {
	mapper := &InoMapper{
		toCpp: map[InoLine]int{},
		toIno: map[int]InoLine{},
	}

	sourceFile := ""
	sourceLine := -1
	targetLine := 0
	scanner := bufio.NewScanner(targetFile)
	for scanner.Scan() {
		lineStr := scanner.Text()
		if strings.HasPrefix(lineStr, "#line") {
			tokens := strings.SplitN(lineStr, " ", 3)
			l, err := strconv.Atoi(tokens[1])
			if err == nil && l > 0 {
				sourceLine = l - 1
			}
			sourceFile = unquoteCppString(tokens[2])
		} else if sourceFile != "" {
			mapper.toCpp[InoLine{sourceFile, sourceLine}] = targetLine
			mapper.toIno[targetLine] = InoLine{sourceFile, sourceLine}
			sourceLine++
		}
		targetLine++
	}
	mapper.toCpp[InoLine{sourceFile, sourceLine}] = targetLine
	mapper.toIno[targetLine] = InoLine{sourceFile, sourceLine}
	return mapper
}

func unquoteCppString(str string) string {
	if len(str) >= 2 && strings.HasPrefix(str, `"`) && strings.HasSuffix(str, `"`) {
		str = strings.TrimSuffix(str, `"`)[1:]
	}
	str = strings.Replace(str, "\\\"", "\"", -1)
	str = strings.Replace(str, "\\\\", "\\", -1)
	return str
}

// Update performs an update to the SourceMap considering the deleted lines, the
// insertion line and the inserted text
func (s *InoMapper) Update(deletedLines, insertLine int, insertText string) {
	// for i := 1; i <= deletedLines; i++ {
	// 	sourceLine := insertLine + 1
	// 	targetLine := s.toCpp[sourceLine]

	// 	// Shift up all following lines by one and put them into a new map
	// 	newMappings := make(map[int]int)
	// 	maxSourceLine, maxTargetLine := 0, 0
	// 	for t, s := range s.toIno {
	// 		if t > targetLine && s > sourceLine {
	// 			newMappings[t-1] = s - 1
	// 		} else if s > sourceLine {
	// 			newMappings[t] = s - 1
	// 		} else if t > targetLine {
	// 			newMappings[t-1] = s
	// 		}
	// 		if s > maxSourceLine {
	// 			maxSourceLine = s
	// 		}
	// 		if t > maxTargetLine {
	// 			maxTargetLine = t
	// 		}
	// 	}

	// 	// Remove mappings for the deleted line
	// 	delete(s.toIno, maxTargetLine)
	// 	delete(s.toCpp, maxSourceLine)

	// 	// Copy the mappings from the intermediate map
	// 	copyMappings(s.toIno, s.toCpp, newMappings)
	// }

	// addedLines := strings.Count(insertText, "\n")
	// if addedLines > 0 {
	// 	targetLine := s.toCpp[insertLine]

	// 	// Shift down all following lines and put them into a new map
	// 	newMappings := make(map[int]int)
	// 	for t, s := range s.toIno {
	// 		if t > targetLine && s > insertLine {
	// 			newMappings[t+addedLines] = s + addedLines
	// 		} else if s > insertLine {
	// 			newMappings[t] = s + addedLines
	// 		} else if t > targetLine {
	// 			newMappings[t+addedLines] = s
	// 		}
	// 	}

	// 	// Add mappings for the added lines
	// 	for i := 1; i <= addedLines; i++ {
	// 		s.toIno[targetLine+i] = insertLine + i
	// 		s.toCpp[insertLine+i] = targetLine + i
	// 	}

	// 	// Copy the mappings from the intermediate map
	// 	copyMappings(s.toIno, s.toCpp, newMappings)
	// }
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
