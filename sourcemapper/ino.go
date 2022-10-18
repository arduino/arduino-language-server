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

package sourcemapper

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/arduino/go-paths-helper"
	"github.com/pkg/errors"
	"go.bug.st/lsp"
	"go.bug.st/lsp/textedits"
)

// SketchMapper is a mapping between the .ino sketch and the preprocessed .cpp file
type SketchMapper struct {
	CppText         *SourceRevision
	inoToCpp        map[InoLine]int // Converts File.ino:line -> line
	cppToIno        map[int]InoLine // Convers line -> File.ino:line
	inoPreprocessed map[InoLine]int // map of the lines taken by the preprocessor: File.ino:line -> preprocessed line
	cppPreprocessed map[int]InoLine // map of the lines added by the preprocessor: preprocessed line -> File.ino:line
}

// NotIno are lines that do not belongs to an .ino file
var NotIno = InoLine{"/not-ino", 0}

// NotInoURI is the DocumentURI that do not belongs to an .ino file
var NotInoURI, _ = lsp.NewDocumentURIFromURL("file:///not-ino")

// SourceRevision is a source code tagged with a version number
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
func (s *SketchMapper) InoToCppLine(sourceURI lsp.DocumentURI, line int) int {
	return s.inoToCpp[InoLine{sourceURI.AsPath().String(), line}]
}

// InoToCppLineOk converts a source (.ino) line into a target (.cpp) line
func (s *SketchMapper) InoToCppLineOk(sourceURI lsp.DocumentURI, line int) (int, bool) {
	res, ok := s.inoToCpp[InoLine{sourceURI.AsPath().String(), line}]
	return res, ok
}

// InoToCppLSPRange convert a lsp.Range reference to a .ino into a lsp.Range to .cpp
func (s *SketchMapper) InoToCppLSPRange(sourceURI lsp.DocumentURI, r lsp.Range) lsp.Range {
	res := r
	res.Start.Line = s.InoToCppLine(sourceURI, r.Start.Line)
	res.End.Line = s.InoToCppLine(sourceURI, r.End.Line)
	return res
}

// InoToCppLSPRangeOk convert a lsp.Range reference to a .ino into a lsp.Range to .cpp and returns
// true if the conversion is successful or false if the conversion is invalid.
func (s *SketchMapper) InoToCppLSPRangeOk(sourceURI lsp.DocumentURI, r lsp.Range) (lsp.Range, bool) {
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
func (s *SketchMapper) CppToInoLine(targetLine int) (string, int) {
	res := s.cppToIno[targetLine]
	return res.File, res.Line
}

// CppToInoRange converts a target (.cpp) lsp.Range into a source.ino:lsp.Range.
// It will panic if the range spans across multiple ino files.
func (s *SketchMapper) CppToInoRange(cppRange lsp.Range) (string, lsp.Range) {
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
func (s *SketchMapper) CppToInoRangeOk(cppRange lsp.Range) (string, lsp.Range, error) {
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
func (s *SketchMapper) CppToInoLineOk(targetLine int) (string, int, bool) {
	res, ok := s.cppToIno[targetLine]
	return res.File, res.Line, ok
}

// IsPreprocessedCppLine returns true if the given .cpp line is part of the
// section added by the arduino preprocessor.
func (s *SketchMapper) IsPreprocessedCppLine(cppLine int) bool {
	_, preprocessed := s.cppPreprocessed[cppLine]
	_, mapsToIno := s.cppToIno[cppLine]
	return preprocessed || !mapsToIno
}

// CreateInoMapper create a InoMapper from the given target file
func CreateInoMapper(targetFile []byte) *SketchMapper {
	mapper := &SketchMapper{
		CppText: &SourceRevision{
			Version: 1,
			Text:    string(targetFile),
		},
	}
	mapper.regeneratehMapping()
	return mapper
}

func (s *SketchMapper) regeneratehMapping() {
	s.inoToCpp = map[InoLine]int{}
	s.cppToIno = map[int]InoLine{}
	s.inoPreprocessed = map[InoLine]int{}
	s.cppPreprocessed = map[int]InoLine{}

	sourceFile := ""
	sourceLine := -1
	targetLine := 0
	scanner := bufio.NewScanner(bytes.NewReader([]byte(s.CppText.Text)))
	for scanner.Scan() {
		lineStr := scanner.Text()
		if strings.HasPrefix(lineStr, "#line") {
			tokens := strings.SplitN(lineStr, " ", 3)
			l, err := strconv.Atoi(tokens[1])
			if err == nil && l > 0 {
				sourceLine = l - 1
			}
			sourceFile = paths.New(unquoteCppString(tokens[2])).Canonical().String()
			s.cppToIno[targetLine] = NotIno
		} else if sourceFile != "" {
			// Sometimes the Arduino preprocessor fails to interpret correctly the code
			// and may report a "#line 0" directive leading to a negative sourceLine.
			// In this rare cases just interpret the source line as a NotIno line.
			if sourceLine >= 0 {
				s.mapLine(sourceFile, sourceLine, targetLine)
			} else {
				s.cppToIno[targetLine] = NotIno
			}
			sourceLine++
		} else {
			s.cppToIno[targetLine] = NotIno
		}
		targetLine++
	}
	s.mapLine(sourceFile, sourceLine, targetLine)
}

func (s *SketchMapper) mapLine(inoSourceFile string, inoSourceLine, cppLine int) {
	inoLine := InoLine{inoSourceFile, inoSourceLine}
	if line, ok := s.inoToCpp[inoLine]; ok {
		s.cppPreprocessed[line] = inoLine
		s.inoPreprocessed[inoLine] = line
	}
	s.inoToCpp[inoLine] = cppLine
	s.cppToIno[cppLine] = inoLine
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
func (s *SketchMapper) ApplyTextChange(inoURI lsp.DocumentURI, inoChange lsp.TextDocumentContentChangeEvent) (dirty bool) {
	inoRange := *inoChange.Range
	cppRange, ok := s.InoToCppLSPRangeOk(inoURI, inoRange)
	if !ok {
		panic("Invalid sketch range " + inoURI.String() + ":" + inoRange.String())
	}
	log.Print("Ino Range: ", inoRange, " -> Cpp Range:", cppRange)
	deletedLines := inoRange.End.Line - inoRange.Start.Line

	// Apply text changes
	newText, err := textedits.ApplyTextChange(s.CppText.Text, cppRange, inoChange.Text)
	if err != nil {
		panic("error replacing text: " + err.Error())
	}
	s.CppText.Text = newText
	s.CppText.Version++

	if _, is := s.inoPreprocessed[s.cppToIno[cppRange.Start.Line]]; is {
		dirty = true
	}

	// Update line references
	for deletedLines > 0 {
		dirty = dirty || s.deleteCppLine(cppRange.Start.Line)
		deletedLines--
	}
	addedLines := strings.Count(inoChange.Text, "\n")
	for addedLines > 0 {
		dirty = dirty || s.addInoLine(cppRange.Start.Line)
		addedLines--
	}
	return
}

func (s *SketchMapper) addInoLine(cppLine int) (dirty bool) {
	preprocessToShiftCpp := map[InoLine]bool{}

	addedInoLine := s.cppToIno[cppLine]
	carry := s.cppToIno[cppLine]
	carry.Line++
	for {
		next, ok := s.cppToIno[cppLine+1]
		s.cppToIno[cppLine+1] = carry
		s.inoToCpp[carry] = cppLine + 1
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
		s.cppToIno[l] = inoPre
	}

	return
}

func (s *SketchMapper) deleteCppLine(line int) (dirty bool) {
	removed := s.cppToIno[line]
	for i := line + 1; ; i++ {
		shifted, ok := s.cppToIno[i]
		if !ok {
			delete(s.cppToIno, i-1)
			break
		}
		s.cppToIno[i-1] = shifted
		if shifted != NotIno {
			s.inoToCpp[shifted] = i - 1
		}
	}

	if _, ok := s.inoPreprocessed[removed]; ok {
		dirty = true
	}

	for curr := removed; ; curr.Line++ {
		next := curr
		next.Line++

		shifted, ok := s.inoToCpp[next]
		if !ok {
			delete(s.inoToCpp, curr)
			break
		}
		s.inoToCpp[curr] = shifted
		s.cppToIno[shifted] = curr

		if l, ok := s.inoPreprocessed[next]; ok {
			s.inoPreprocessed[curr] = l
			s.cppPreprocessed[l] = curr
			delete(s.inoPreprocessed, next)

			s.cppToIno[l] = curr
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
func (s *SketchMapper) DebugLogAll() {
	stripFile := func(s string) string {
		l := strings.LastIndex(s, "/")
		if l == -1 {
			return s
		}
		return s[l:]
	}
	cpp := strings.Split(s.CppText.Text, "\n")
	log.Printf("  > Current sketchmapper content:")
	for l, cppLine := range cpp {
		inoFile, inoLine := s.CppToInoLine(l)
		cppLine = strings.Replace(cppLine, "\t", "  ", -1)
		if len(cppLine) > 60 {
			cppLine = cppLine[:60]
		}

		cppSource := fmt.Sprintf("%3d: %-60s", l, cppLine)
		sketchFile := fmt.Sprintf("%s:%d", stripFile(inoFile), inoLine)
		preprocLine := ""
		if pr, ok := s.cppPreprocessed[l]; ok {
			preprocLine = fmt.Sprintf("%s:%d", stripFile(pr.File), pr.Line)
		}
		log.Printf("%s | %-25s %-25s", cppSource, sketchFile, preprocLine)
	}
}
