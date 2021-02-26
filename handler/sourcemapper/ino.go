package sourcemapper

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/arduino/arduino-language-server/handler/textutils"
	"github.com/arduino/arduino-language-server/lsp"
	"github.com/arduino/go-paths-helper"
	"github.com/pkg/errors"
)

// InoMapper is a mapping between the .ino sketch and the preprocessed .cpp file
type InoMapper struct {
	CppText         *SourceRevision
	toCpp           map[InoLine]int // Converts File.ino:line -> line
	toIno           map[int]InoLine // Convers line -> File.ino:line
	inoPreprocessed map[InoLine]int // map of the lines taken by the preprocessor: File.ino:line -> preprocessed line
	cppPreprocessed map[int]InoLine // map of the lines added by the preprocessor: preprocessed line -> File.ino:line
}

// NotIno are lines that do not belongs to an .ino file
var NotIno = InoLine{"/not-ino", 0}

// NotInoURI is the DocumentURI that do not belongs to an .ino file
var NotInoURI, _ = lsp.NewDocumentURIFromURL("file:///not-ino")

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
	return s.toCpp[InoLine{sourceURI.AsPath().String(), line}]
}

// InoToCppLineOk converts a source (.ino) line into a target (.cpp) line
func (s *InoMapper) InoToCppLineOk(sourceURI lsp.DocumentURI, line int) (int, bool) {
	res, ok := s.toCpp[InoLine{sourceURI.AsPath().String(), line}]
	return res, ok
}

// InoToCppLSPRange convert a lsp.Ranger reference to a .ino into a lsp.Range to .cpp
func (s *InoMapper) InoToCppLSPRange(sourceURI lsp.DocumentURI, r lsp.Range) lsp.Range {
	res := r
	res.Start.Line = s.InoToCppLine(sourceURI, r.Start.Line)
	res.End.Line = s.InoToCppLine(sourceURI, r.End.Line)
	return res
}

// InoToCppLSPRangeOk convert a lsp.Ranger reference to a .ino into a lsp.Range to .cpp and returns
// true if the conversion is successful or false if the conversion is invalid.
func (s *InoMapper) InoToCppLSPRangeOk(sourceURI lsp.DocumentURI, r lsp.Range) (lsp.Range, bool) {
	res := r
	if l, ok := s.InoToCppLineOk(sourceURI, r.Start.Line); ok {
		res.Start.Line = l
	} else {
		return res, false
	}
	if l, ok := s.InoToCppLineOk(sourceURI, r.End.Line); ok {
		res.End.Line = l
	} else {
		return res, false
	}
	return res, true
}

// CppToInoLine converts a target (.cpp) line into a source.ino:line
func (s *InoMapper) CppToInoLine(targetLine int) (string, int) {
	res := s.toIno[targetLine]
	return res.File, res.Line
}

// CppToInoRange converts a target (.cpp) lsp.Range into a source.ino:lsp.Range.
// It will panic if the range spans across multiple ino files.
func (s *InoMapper) CppToInoRange(cppRange lsp.Range) (string, lsp.Range) {
	inoFile, inoRange, err := s.CppToInoRangeOk(cppRange)
	if err != nil {
		panic(err.Error())
	}
	return inoFile, inoRange
}

// AdjustedRangeErr is returned if the range overlaps with a non-ino section by just the
// last newline character.
type AdjustedRangeErr struct{}

func (e AdjustedRangeErr) Error() string {
	return "the range has been adjusted to allow final newline"
}

// CppToInoRangeOk converts a target (.cpp) lsp.Range into a source.ino:lsp.Range.
// It returns an error if the range spans across multiple ino files.
// If the range ends on the beginning of a new line in another .ino file, the range
// is adjusted and AdjustedRangeErr is reported as err: the range may be still valid.
func (s *InoMapper) CppToInoRangeOk(cppRange lsp.Range) (string, lsp.Range, error) {
	inoFile, startLine := s.CppToInoLine(cppRange.Start.Line)
	endInoFile, endLine := s.CppToInoLine(cppRange.End.Line)
	inoRange := cppRange
	inoRange.Start.Line = startLine
	inoRange.End.Line = endLine
	if inoFile == endInoFile {
		// All done
		return inoFile, inoRange, nil
	}

	// Special case: the last line ends up in the "not-ino" area
	if inoRange.End.Character == 0 {
		if checkFile, checkLine := s.CppToInoLine(cppRange.End.Line - 1); checkFile == inoFile {
			// Adjust the range and return it with an AdjustedRange notification
			inoRange.End.Line = checkLine + 1
			return inoFile, inoRange, AdjustedRangeErr{}
		}
	}

	// otherwise the range is not recoverable, just report error
	return inoFile, inoRange, errors.Errorf("invalid range conversion %s -> %s:%d-%s:%d", cppRange, inoFile, startLine, endInoFile, endLine)
}

// CppToInoLineOk converts a target (.cpp) line into a source (.ino) line and
// returns true if the conversion is successful
func (s *InoMapper) CppToInoLineOk(targetLine int) (string, int, bool) {
	res, ok := s.toIno[targetLine]
	return res.File, res.Line, ok
}

// IsPreprocessedCppLine returns true if the give .cpp line is part of the
// section added by the arduino preprocessor.
func (s *InoMapper) IsPreprocessedCppLine(cppLine int) bool {
	_, preprocessed := s.cppPreprocessed[cppLine]
	_, mapsToIno := s.toIno[cppLine]
	return preprocessed || !mapsToIno
}

// CreateInoMapper create a InoMapper from the given target file
func CreateInoMapper(targetFile []byte) *InoMapper {
	mapper := &InoMapper{
		toCpp:           map[InoLine]int{},
		toIno:           map[int]InoLine{},
		inoPreprocessed: map[InoLine]int{},
		cppPreprocessed: map[int]InoLine{},
		CppText: &SourceRevision{
			Version: 1,
			Text:    string(targetFile),
		},
	}

	sourceFile := ""
	sourceLine := -1
	targetLine := 0
	scanner := bufio.NewScanner(bytes.NewReader(targetFile))
	for scanner.Scan() {
		lineStr := scanner.Text()
		if strings.HasPrefix(lineStr, "#line") {
			tokens := strings.SplitN(lineStr, " ", 3)
			l, err := strconv.Atoi(tokens[1])
			if err == nil && l > 0 {
				sourceLine = l - 1
			}
			sourceFile = paths.New(unquoteCppString(tokens[2])).Canonical().String()
			mapper.toIno[targetLine] = NotIno
		} else if sourceFile != "" {
			mapper.mapLine(sourceFile, sourceLine, targetLine)
			sourceLine++
		} else {
			mapper.toIno[targetLine] = NotIno
		}
		targetLine++
	}
	mapper.mapLine(sourceFile, sourceLine, targetLine)
	return mapper
}

func (s *InoMapper) mapLine(sourceFile string, sourceLine, targetLine int) {
	inoLine := InoLine{sourceFile, sourceLine}
	if line, ok := s.toCpp[inoLine]; ok {
		s.cppPreprocessed[line] = inoLine
		s.inoPreprocessed[inoLine] = line
	}
	s.toCpp[inoLine] = targetLine
	s.toIno[targetLine] = inoLine
}

func unquoteCppString(str string) string {
	if len(str) >= 2 && strings.HasPrefix(str, `"`) && strings.HasSuffix(str, `"`) {
		str = strings.TrimSuffix(str, `"`)[1:]
	}
	str = strings.Replace(str, "\\\"", "\"", -1)
	str = strings.Replace(str, "\\\\", "\\", -1)
	return str
}

// ApplyTextChange performs the text change and updates both .ino and .cpp files.
// It returns true if the change is "dirty", this happens when the change alters preprocessed lines
// and a new preprocessing may be probably required.
func (s *InoMapper) ApplyTextChange(inoURI lsp.DocumentURI, inoChange lsp.TextDocumentContentChangeEvent) (dirty bool) {
	inoRange := *inoChange.Range
	cppRange := s.InoToCppLSPRange(inoURI, inoRange)
	deletedLines := inoRange.End.Line - inoRange.Start.Line

	// Apply text changes
	newText, err := textutils.ApplyTextChange(s.CppText.Text, cppRange, inoChange.Text)
	if err != nil {
		panic("error replacing text: " + err.Error())
	}
	s.CppText.Text = newText
	s.CppText.Version++

	if _, is := s.inoPreprocessed[s.toIno[cppRange.Start.Line]]; is {
		dirty = true
	}

	// Update line references
	for deletedLines > 0 {
		dirty = dirty || s.deleteCppLine(cppRange.Start.Line)
		deletedLines--
	}
	addedLines := strings.Count(inoChange.Text, "\n") - 1
	for addedLines > 0 {
		dirty = dirty || s.addInoLine(cppRange.Start.Line)
		addedLines--
	}
	return
}

func (s *InoMapper) addInoLine(cppLine int) (dirty bool) {
	preprocessToShiftCpp := map[InoLine]bool{}

	addedInoLine := s.toIno[cppLine]
	carry := s.toIno[cppLine]
	carry.Line++
	for {
		next, ok := s.toIno[cppLine+1]
		s.toIno[cppLine+1] = carry
		s.toCpp[carry] = cppLine + 1
		if !ok {
			break
		}

		if next.File == addedInoLine.File && next.Line >= addedInoLine.Line {
			if _, is := s.inoPreprocessed[next]; is {
				// fmt.Println("Adding", next, "to cpp to shift")
				preprocessToShiftCpp[next] = true
			}
			next.Line++
		}

		carry = next
		cppLine++
	}

	// dumpCppToInoMap(s.toIno)

	preprocessToShiftIno := []InoLine{}
	for inoPre := range s.inoPreprocessed {
		// fmt.Println(">", inoPre, addedInoLine)
		if inoPre.File == addedInoLine.File && inoPre.Line >= addedInoLine.Line {
			preprocessToShiftIno = append(preprocessToShiftIno, inoPre)
		}
	}
	for inoPre := range preprocessToShiftCpp {
		l := s.inoPreprocessed[inoPre]
		delete(s.cppPreprocessed, l)
		s.inoPreprocessed[inoPre] = l + 1
		s.cppPreprocessed[l+1] = inoPre
	}
	for _, inoPre := range preprocessToShiftIno {
		l := s.inoPreprocessed[inoPre]
		delete(s.inoPreprocessed, inoPre)
		inoPre.Line++
		s.inoPreprocessed[inoPre] = l
		s.cppPreprocessed[l] = inoPre
		s.toIno[l] = inoPre
	}

	return
}

func (s *InoMapper) deleteCppLine(line int) (dirty bool) {
	removed := s.toIno[line]
	for i := line + 1; ; i++ {
		shifted, ok := s.toIno[i]
		if !ok {
			delete(s.toIno, i-1)
			break
		}
		s.toIno[i-1] = shifted
		if shifted != NotIno {
			s.toCpp[shifted] = i - 1
		}
	}

	if _, ok := s.inoPreprocessed[removed]; ok {
		dirty = true
	}

	for curr := removed; ; curr.Line++ {
		next := curr
		next.Line++

		shifted, ok := s.toCpp[next]
		if !ok {
			delete(s.toCpp, curr)
			break
		}
		s.toCpp[curr] = shifted
		s.toIno[shifted] = curr

		if l, ok := s.inoPreprocessed[next]; ok {
			s.inoPreprocessed[curr] = l
			s.cppPreprocessed[l] = curr
			delete(s.inoPreprocessed, next)

			s.toIno[l] = curr
		}
	}
	return
}

func dumpCppToInoMap(s map[int]InoLine) {
	last := 0
	for cppLine := range s {
		if last < cppLine {
			last = cppLine
		}
	}
	for line := 0; line <= last; line++ {
		target := s[line]
		fmt.Printf("%5d -> %s:%d\n", line, target.File, target.Line)
	}
}

func dumpInoToCppMap(s map[InoLine]int) {
	keys := []InoLine{}
	for k := range s {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].File < keys[j].File ||
			(keys[i].File == keys[j].File && keys[i].Line < keys[j].Line)
	})
	for _, k := range keys {
		inoLine := k
		cppLine := s[inoLine]
		fmt.Printf("%s:%d -> %d\n", inoLine.File, inoLine.Line, cppLine)
	}
}

// DebugLogAll dumps the internal status of the mapper
func (s *InoMapper) DebugLogAll() {
	cpp := strings.Split(s.CppText.Text, "\n")
	log.Printf("  > Current sketchmapper content:")
	for l, cppLine := range cpp {
		inoFile, inoLine := s.CppToInoLine(l)
		log.Printf("  %3d: %-40s : %s:%d", l, cppLine, inoFile, inoLine)
	}
}
