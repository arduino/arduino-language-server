package sourcemapper

import (
	"reflect"
	"strings"
	"testing"
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
	sourceMap := CreateInoMapper(strings.NewReader(input))
	if !reflect.DeepEqual(sourceMap.toIno, map[int]int{
		3:  0,
		5:  1,
		7:  6,
		9:  1,
		10: 2,
		11: 3,
		12: 4,
		13: 5,
		14: 6,
		15: 7,
		16: 8,
		17: 9,
		18: 10,
	}) {
		t.Error(sourceMap.toIno)
	}
	if !reflect.DeepEqual(sourceMap.toCpp, map[int]int{
		0:  3,
		1:  9,
		2:  10,
		3:  11,
		4:  12,
		5:  13,
		6:  14,
		7:  15,
		8:  16,
		9:  17,
		10: 18},
	) {
		t.Error(sourceMap.toCpp)
	}
}

func TestUpdateSourceMaps1(t *testing.T) {
	sourceMap := &InoMapper{
		toCpp: map[int]int{
			0: 1,
			1: 2,
			2: 0,
			3: 5,
			4: 3,
			5: 4,
		},
		toIno: make(map[int]int),
	}
	for s, t := range sourceMap.toCpp {
		sourceMap.toIno[t] = s
	}
	sourceMap.Update(0, 1, "foo\nbar\nbaz")
	if !reflect.DeepEqual(sourceMap.toCpp, map[int]int{
		0: 1,
		1: 2,
		2: 3,
		3: 4,
		4: 0,
		5: 7,
		6: 5,
		7: 6},
	) {
		t.Error(sourceMap.toCpp)
	}
	if !reflect.DeepEqual(sourceMap.toIno, map[int]int{
		0: 4,
		1: 0,
		2: 1,
		3: 2,
		4: 3,
		5: 6,
		6: 7,
		7: 5},
	) {
		t.Error(sourceMap.toIno)
	}
}

func TestUpdateSourceMaps2(t *testing.T) {
	sourceMap := &InoMapper{
		toCpp: map[int]int{
			0: 1,
			1: 2,
			2: 0,
			3: 5,
			4: 3,
			5: 4},
		toIno: make(map[int]int),
	}
	for s, t := range sourceMap.toCpp {
		sourceMap.toIno[t] = s
	}
	sourceMap.Update(2, 1, "foo")
	if !reflect.DeepEqual(sourceMap.toCpp, map[int]int{
		0: 0,
		1: 1,
		2: 2,
		3: 3},
	) {
		t.Error(sourceMap.toCpp)
	}
	if !reflect.DeepEqual(sourceMap.toIno, map[int]int{
		0: 0,
		1: 1,
		2: 2,
		3: 3},
	) {
		t.Error(sourceMap.toIno)
	}
}
