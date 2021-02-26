package textutils

import (
	"strings"
	"testing"

	"github.com/arduino/arduino-language-server/lsp"
)

func TestApplyTextChange(t *testing.T) {
	tests := []struct {
		InitialText string
		Range       lsp.Range
		Insertion   string
		Expectation string
		Err         error
	}{
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 2, Character: 2},
			},
			"i",
			"foo\nbiz\n!",
			nil,
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 1, Character: 2},
			},
			"ee",
			"foo\nbeer\nbaz\n!",
			nil,
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 1, Character: 1},
			},
			"eer from the st",
			"foo\nbeer from the star\nbaz\n!",
			nil,
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				Start: lsp.Position{Line: 0, Character: 10},
				End:   lsp.Position{Line: 2, Character: 20},
			},
			"i",
			"fooi\n!",
			nil,
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				Start: lsp.Position{Line: 0, Character: 100},
				End:   lsp.Position{Line: 2, Character: 0},
			},
			"i",
			"fooibaz\n!",
			nil,
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				// out of range start offset
				Start: lsp.Position{Line: 20, Character: 0},
				End:   lsp.Position{Line: 2, Character: 0},
			},
			"i",
			"",
			OutOfRangeError{"Line", 3, 20},
		},
		{
			"foo\nbar\nbaz\n!",
			lsp.Range{
				// out of range end offset
				Start: lsp.Position{Line: 0, Character: 0},
				End:   lsp.Position{Line: 20, Character: 0},
			},
			"i",
			"",
			OutOfRangeError{"Line", 3, 20},
		},
	}

	for _, test := range tests {
		initial := strings.ReplaceAll(test.InitialText, "\n", "\\n")
		insertion := strings.ReplaceAll(test.Insertion, "\n", "\\n")
		expectation := strings.ReplaceAll(test.Expectation, "\n", "\\n")

		t.Logf("applyTextChange(\"%s\", %v, \"%s\") == \"%s\"", initial, test.Range, insertion, expectation)
		act, err := ApplyTextChange(test.InitialText, test.Range, test.Insertion)
		if act != test.Expectation {
			t.Errorf("applyTextChange(\"%s\", %v, \"%s\") != \"%s\", got \"%s\"", initial, test.Range, insertion, expectation, strings.ReplaceAll(act, "\n", "\\n"))
		}
		if err != test.Err {
			t.Errorf("applyTextChange(\"%s\", %v, \"%s\") error != %v, got %v instead", initial, test.Range, insertion, test.Err, err)
		}
	}
}

func TestGetOffset(t *testing.T) {
	tests := []struct {
		Text string
		Line int
		Char int
		Exp  int
		Err  error
	}{
		{"foo\nfoobar\nbaz", 0, 0, 0, nil},
		{"foo\nfoobar\nbaz", 1, 0, 4, nil},
		{"foo\nfoobar\nbaz", 1, 3, 7, nil},
		{"foo\nba\nr\nbaz\n!", 3, 0, 9, nil},
		{"foo\nba\nr\nbaz\n!", 1, 10, 6, nil},
		{"foo\nba\nr\nbaz\n!", -1, 0, -1, OutOfRangeError{"Line", 4, -1}},
		{"foo\nba\nr\nbaz\n!", 1, -1, -1, OutOfRangeError{"Character", 2, -1}},
		{"foo\nba\nr\nbaz\n!", 4, 20, 14, nil},
		{"foo\nba\nr\nbaz!\n", 4, 0, 14, nil},
	}

	for _, test := range tests {
		st := strings.Replace(test.Text, "\n", "\\n", -1)

		t.Logf("getOffset(\"%s\", {Line: %d, Character: %d}) == %d", st, test.Line, test.Char, test.Exp)
		act, err := GetOffset(test.Text, lsp.Position{Line: test.Line, Character: test.Char})
		if act != test.Exp {
			t.Errorf("getOffset(\"%s\", {Line: %d, Character: %d}) != %d, got %d instead", st, test.Line, test.Char, test.Exp, act)
		}
		if err != test.Err {
			t.Errorf("getOffset(\"%s\", {Line: %d, Character: %d}) error != %v, got %v instead", st, test.Line, test.Char, test.Err, err)
		}
	}
}

func TestGetLineOffset(t *testing.T) {
	tests := []struct {
		Text string
		Line int
		Exp  int
		Err  error
	}{
		{"foo\nfoobar\nbaz", 0, 0, nil},
		{"foo\nfoobar\nbaz", 1, 4, nil},
		{"foo\nfoobar\nbaz", 2, 11, nil},
		{"foo\nfoobar\nbaz", 3, -1, OutOfRangeError{"Line", 2, 3}},
		{"foo\nba\nr\nbaz\n!", 3, 9, nil},
		{"foo\nba\nr\nbaz\n!", -1, -1, OutOfRangeError{"Line", 4, -1}},
		{"foo\nba\nr\nbaz\n!", 20, -1, OutOfRangeError{"Line", 4, 20}},
	}

	for _, test := range tests {
		st := strings.Replace(test.Text, "\n", "\\n", -1)

		t.Logf("getLineOffset(\"%s\", %d) == %d", st, test.Line, test.Exp)
		act, err := GetLineOffset(test.Text, test.Line)
		if act != test.Exp {
			t.Errorf("getLineOffset(\"%s\", %d) != %d, got %d instead", st, test.Line, test.Exp, act)
		}
		if err != test.Err {
			t.Errorf("getLineOffset(\"%s\", %d) error != %v, got %v instead", st, test.Line, test.Err, err)
		}
	}
}
