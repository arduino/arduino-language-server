package handler

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	lsp "github.com/sourcegraph/go-lsp"
)

func createSourceMaps(targetFile io.Reader) (sourceLineMap, targetLineMap map[int]int) {
	sourceLine := -1
	targetLine := 0
	sourceLineMap = make(map[int]int)
	targetLineMap = make(map[int]int)
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
	return
}

func updateSourceMaps(sourceLineMap, targetLineMap map[int]int, deletedLines, insertLine int, insertText string) {
	for i := 1; i <= deletedLines; i++ {
		sourceLine := insertLine + 1
		targetLine := targetLineMap[sourceLine]

		// Shift up all following lines by one and put them into a new map
		newMappings := make(map[int]int)
		maxSourceLine, maxTargetLine := 0, 0
		for t, s := range sourceLineMap {
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
		delete(sourceLineMap, maxTargetLine)
		delete(targetLineMap, maxSourceLine)

		// Copy the mappings from the intermediate map
		copyMappings(sourceLineMap, targetLineMap, newMappings)
	}

	addedLines := strings.Count(insertText, "\n")
	if addedLines > 0 {
		targetLine := targetLineMap[insertLine]

		// Shift down all following lines and put them into a new map
		newMappings := make(map[int]int)
		for t, s := range sourceLineMap {
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
			sourceLineMap[targetLine+i] = insertLine + i
			targetLineMap[insertLine+i] = targetLine + i
		}

		// Copy the mappings from the intermediate map
		copyMappings(sourceLineMap, targetLineMap, newMappings)
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

// OutOfRangeError returned if one attempts to access text out of its range
type OutOfRangeError struct {
	Max int
	Req lsp.Position
}

func (oor OutOfRangeError) Error() string {
	return fmt.Sprintf("text access out of range: max=%d requested=%d", oor.Max, oor.Req)
}

func applyTextChange(text string, rang lsp.Range, insertText string) (res string, err error) {
	start, err := getOffset(text, rang.Start)
	if err != nil {
		return "", err
	}
	end, err := getOffset(text, rang.End)
	if err != nil {
		return "", err
	}

	return text[:start] + insertText + text[end:], nil
}

// getOffset computes the offset in the text expressed by the lsp.Position.
// Returns OutOfRangeError if the position is out of range.
func getOffset(text string, pos lsp.Position) (off int, err error) {
	// find line
	lineOffset := getLineOffset(text, pos.Line)
	if lineOffset < 0 {
		return -1, OutOfRangeError{len(text), pos}
	}
	off = lineOffset

	// walk towards the character
	var charFound bool
	for offset, c := range text[off:] {
		if c == '\n' {
			// We've reached the end of line. LSP spec says we should default back to the line length.
			// See https://microsoft.github.io/language-server-protocol/specifications/specification-3-14/#position
			off += offset
			charFound = true
			break
		}

		// we've fond the character
		if offset == pos.Character {
			off += offset
			charFound = true
			break
		}
	}
	if !charFound {
		return -1, OutOfRangeError{Max: len(text), Req: pos}
	}

	return off, nil
}

// getLineOffset finds the offset/position of the beginning of a line within the text.
// For example:
//    text := "foo\nfoobar\nbaz"
//    getLineOffset(text, 0) == 0
//    getLineOffset(text, 1) == 4
//    getLineOffset(text, 2) == 11
func getLineOffset(text string, line int) int {
	if line < 0 {
		return -1
	}
	if line == 0 {
		return 0
	}

	// find the line and return its offset within the text
	var count int
	for offset, c := range text {
		if c != '\n' {
			continue
		}

		count++
		if count == line {
			return offset + 1
		}
	}

	// we didn't find the line in the text
	return -1
}
