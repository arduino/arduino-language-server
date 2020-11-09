package textutils

import (
	"fmt"

	"github.com/sourcegraph/go-lsp"
)

// ApplyTextChange replaces startingText substring specified by replaceRange with insertText
func ApplyTextChange(startingText string, replaceRange lsp.Range, insertText string) (res string, err error) {
	start, err := getOffset(startingText, replaceRange.Start)
	if err != nil {
		return "", err
	}
	end, err := getOffset(startingText, replaceRange.End)
	if err != nil {
		return "", err
	}

	return startingText[:start] + insertText + startingText[end:], nil
}

// getOffset computes the offset in the text expressed by the lsp.Position.
// Returns OutOfRangeError if the position is out of range.
func getOffset(text string, pos lsp.Position) (int, error) {
	// Find line
	lineOffset, err := getLineOffset(text, pos.Line)
	if err != nil {
		return -1, err
	}
	character := pos.Character
	if character == 0 {
		return lineOffset, nil
	}

	// Find the character and return its offset within the text
	var count = len(text[lineOffset:])
	for offset, c := range text[lineOffset:] {
		if character == offset {
			// We've found the character
			return lineOffset + offset, nil
		}
		if c == '\n' {
			// We've reached the end of line. LSP spec says we should default back to the line length.
			// See https://microsoft.github.io/language-server-protocol/specifications/specification-3-14/#position
			if character > offset {
				return lineOffset + offset, nil
			}
			count = offset
			break
		}
	}
	if character > 0 {
		// We've reached the end of the last line. Default to the text length (see above).
		return len(text), nil
	}

	// We haven't found the character in the text (character index was negative)
	return -1, OutOfRangeError{"Character", count, character}
}

// getLineOffset finds the offset/position of the beginning of a line within the text.
// For example:
//    text := "foo\nfoobar\nbaz"
//    getLineOffset(text, 0) == 0
//    getLineOffset(text, 1) == 4
//    getLineOffset(text, 2) == 11
func getLineOffset(text string, line int) (int, error) {
	if line == 0 {
		return 0, nil
	}

	// Find the line and return its offset within the text
	var count int
	for offset, c := range text {
		if c == '\n' {
			count++
			if count == line {
				return offset + 1, nil
			}
		}
	}

	// We haven't found the line in the text
	return -1, OutOfRangeError{"Line", count, line}
}

// OutOfRangeError returned if one attempts to access text out of its range
type OutOfRangeError struct {
	Type string
	Max  int
	Req  int
}

func (oor OutOfRangeError) Error() string {
	return fmt.Sprintf("%s access out of range: max=%d requested=%d", oor.Type, oor.Max, oor.Req)
}
