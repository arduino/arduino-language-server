package main

import (
	"reflect"
	"strings"
	"testing"

	lsp "github.com/sourcegraph/go-lsp"
)

func TestCreateSourceMaps(t *testing.T) {
	input := `#include <Arduino.h>
#line 1 "sketch_july2a.ino"
#line 1 "sketch_july2a.ino"

#line 2 "sketch_july2a.ino"
void setup();
#line 7 "sketch_july2a.ino"
void loop();
#line 2 "sketch_july2a.ino"
void setup() {
	// put your setup code here, to run once:
	
}

void loop() {
	// put your main code here, to run repeatedly:
	
}
`
	sourceLineMap, targetLineMap := createSourceMaps(strings.NewReader(input))
	if !reflect.DeepEqual(sourceLineMap, map[int]int{
		3: 0, 5: 1, 7: 6, 9: 1, 10: 2, 11: 3, 12: 4, 13: 5, 14: 6, 15: 7, 16: 8, 17: 9, 18: 10,
	}) {
		t.Error(sourceLineMap)
	}
	if !reflect.DeepEqual(targetLineMap, map[int]int{
		0: 3, 1: 9, 2: 10, 3: 11, 4: 12, 5: 13, 6: 14, 7: 15, 8: 16, 9: 17, 10: 18,
	}) {
		t.Error(targetLineMap)
	}
}

func TestUpdateSourceMaps(t *testing.T) {
	targetLineMap := map[int]int{0: 1, 1: 2, 2: 0, 3: 5, 4: 3, 5: 4}
	sourceLineMap := make(map[int]int)
	for s, t := range targetLineMap {
		sourceLineMap[t] = s
	}
	updateSourceMaps(sourceLineMap, targetLineMap, 1, "foo\nbar\nbaz")
	if !reflect.DeepEqual(targetLineMap, map[int]int{0: 1, 1: 2, 2: 3, 3: 4, 4: 0, 5: 7, 6: 5, 7: 6}) {
		t.Error(targetLineMap)
	}
	if !reflect.DeepEqual(sourceLineMap, map[int]int{0: 4, 1: 0, 2: 1, 3: 2, 4: 3, 5: 6, 6: 7, 7: 5}) {
		t.Error(sourceLineMap)
	}
}

func TestApplyTextChange(t *testing.T) {
	text1 := applyTextChange("foo\nbar\nbaz\n!", lsp.Range{
		Start: lsp.Position{Line: 1, Character: 1},
		End:   lsp.Position{Line: 2, Character: 2},
	}, "i")
	if text1 != "foo\nbiz\n!" {
		t.Error(text1)
	}
	text2 := applyTextChange("foo\nbar\nbaz\n!", lsp.Range{
		Start: lsp.Position{Line: 1, Character: 1},
		End:   lsp.Position{Line: 1, Character: 2},
	}, "ee")
	if text2 != "foo\nbeer\nbaz\n!" {
		t.Error(text2)
	}
	text3 := applyTextChange("foo\nbar\nbaz\n!", lsp.Range{
		Start: lsp.Position{Line: 1, Character: 1},
		End:   lsp.Position{Line: 1, Character: 1},
	}, "eer from the st")
	if text3 != "foo\nbeer from the star\nbaz\n!" {
		t.Error(text3)
	}
}

func TestGetLineOffset(t *testing.T) {
	offset := getLineOffset("foo\nba\nr\nbaz\n!", 3)
	if offset != 9 {
		t.Error(offset)
	}
}
