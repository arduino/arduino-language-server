package handler

import (
	"bufio"
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

func updateSourceMaps(sourceLineMap, targetLineMap map[int]int, insertLine int, insertText string) {
	addedLines := strings.Count(insertText, "\n")
	if addedLines > 0 {
		targetLine := targetLineMap[insertLine]

		// Shift all following lines and put them into a new map
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
}

func applyTextChange(text string, rang lsp.Range, insertText string) string {
	if rang.Start.Line != rang.End.Line {
		startOffset := getLineOffset(text, rang.Start.Line) + rang.Start.Character
		endOffset := getLineOffset(text, rang.End.Line) + rang.End.Character
		return text[:startOffset] + insertText + text[endOffset:]
	} else if rang.Start.Character != rang.End.Character {
		lineOffset := getLineOffset(text, rang.Start.Line)
		startOffset := lineOffset + rang.Start.Character
		endOffset := lineOffset + rang.End.Character
		return text[:startOffset] + insertText + text[endOffset:]
	} else {
		offset := getLineOffset(text, rang.Start.Line) + rang.Start.Character
		return text[:offset] + insertText + text[offset:]
	}
}

func getLineOffset(text string, line int) int {
	if line == 0 {
		return 0
	}
	count := 0
	for offset, c := range text {
		if c == '\n' {
			count++
			if count == line {
				return offset + 1
			}
		}
	}
	return len(text)
}
