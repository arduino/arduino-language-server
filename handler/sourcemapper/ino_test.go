package sourcemapper

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
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
	sourceMap := CreateInoMapper([]byte(input))
	require.EqualValues(t, map[InoLine]int{
		{"sketch_july2a.ino", 0}:  3,
		{"sketch_july2a.ino", 1}:  9,
		{"sketch_july2a.ino", 2}:  10,
		{"sketch_july2a.ino", 3}:  11,
		{"sketch_july2a.ino", 4}:  12,
		{"sketch_july2a.ino", 5}:  13,
		{"sketch_july2a.ino", 6}:  14,
		{"sketch_july2a.ino", 7}:  15,
		{"sketch_july2a.ino", 8}:  16,
		{"sketch_july2a.ino", 9}:  17,
		{"sketch_july2a.ino", 10}: 18,
	}, sourceMap.toCpp)
	require.EqualValues(t, map[int]InoLine{
		0:  NotIno,
		1:  NotIno,
		2:  NotIno,
		3:  {"sketch_july2a.ino", 0},
		4:  NotIno,
		5:  {"sketch_july2a.ino", 1}, // setup
		6:  NotIno,
		7:  {"sketch_july2a.ino", 6}, // loop
		8:  NotIno,
		9:  {"sketch_july2a.ino", 1},
		10: {"sketch_july2a.ino", 2},
		11: {"sketch_july2a.ino", 3},
		12: {"sketch_july2a.ino", 4},
		13: {"sketch_july2a.ino", 5},
		14: {"sketch_july2a.ino", 6},
		15: {"sketch_july2a.ino", 7},
		16: {"sketch_july2a.ino", 8},
		17: {"sketch_july2a.ino", 9},
		18: {"sketch_july2a.ino", 10},
	}, sourceMap.toIno)
	require.EqualValues(t, map[int]InoLine{
		5: {"sketch_july2a.ino", 1}, // setup
		7: {"sketch_july2a.ino", 6}, // loop
	}, sourceMap.cppPreprocessed)

	dumpCppToInoMap(sourceMap.toIno)
	dumpInoToCppMap(sourceMap.toCpp)
	dumpCppToInoMap(sourceMap.cppPreprocessed)
	dumpInoToCppMap(sourceMap.inoPreprocessed)
	//sourceMap.addInoLine(InoLine{"sketch_july2a.ino", 0})
	sourceMap.addInoLine(3)
	fmt.Println("\nAdded line 13")
	dumpCppToInoMap(sourceMap.toIno)
	dumpInoToCppMap(sourceMap.toCpp)
	dumpCppToInoMap(sourceMap.cppPreprocessed)
	dumpInoToCppMap(sourceMap.inoPreprocessed)
}

func TestCreateMultifileSourceMap(t *testing.T) {
	input := `#include <Arduino.h>
#line 1 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino"
#include <SPI.h>
#include <Audio.h>

#line 4 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino"
void setup();
#line 9 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino"
void loop();
#line 23 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino"
void vino();
#line 2 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino"
void secondFunction();
#line 4 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino"
void setup() {
  // put your setup code here, to run once:
  digitalWrite(10, 20);
}

void loop() {
  // put your main code here, to run repeatedly:
  long pippo = Serial.available();
  pippo++;
  Serial1.write(pippo);
  SPI.begin();
  int ciao = millis();
  Serial.println(ciao, HEX);
  if (ciao > 10) {
	SerialUSB.println();
  }
  Serial.println();
}

void vino() {
}

#line 1 "/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino"

void secondFunction() {

}`
	sourceMap := CreateInoMapper([]byte(input))
	require.EqualValues(t, sourceMap.toCpp, map[InoLine]int{
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 0}:  2,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 1}:  3,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 2}:  4,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 3}:  14,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 4}:  15,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 5}:  16,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 6}:  17,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 7}:  18,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 8}:  19,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 9}:  20,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 10}: 21,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 11}: 22,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 12}: 23,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 13}: 24,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 14}: 25,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 15}: 26,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 16}: 27,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 17}: 28,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 18}: 29,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 19}: 30,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 20}: 31,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 21}: 32,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 22}: 33,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 23}: 34,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 24}: 35,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 0}:     37,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 1}:     38,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 2}:     39,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 3}:     40,
		{"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 4}:     41,
	})
	require.EqualValues(t, sourceMap.toIno, map[int]InoLine{
		0:  NotIno,
		1:  NotIno,
		2:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 0},
		3:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 1},
		4:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 2},
		5:  NotIno,
		6:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 3}, // setup
		7:  NotIno,
		8:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 8}, // loop
		9:  NotIno,
		10: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 22}, // vino
		11: NotIno,
		12: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 1}, // secondFunction
		13: NotIno,
		14: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 3},
		15: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 4},
		16: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 5},
		17: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 6},
		18: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 7},
		19: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 8},
		20: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 9},
		21: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 10},
		22: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 11},
		23: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 12},
		24: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 13},
		25: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 14},
		26: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 15},
		27: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 16},
		28: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 17},
		29: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 18},
		30: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 19},
		31: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 20},
		32: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 21},
		33: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 22},
		34: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 23},
		35: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 24},
		36: {"not-ino", 0},
		37: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 0},
		38: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 1},
		39: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 2},
		40: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 3},
		41: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 4},
	})
	require.EqualValues(t, map[int]InoLine{
		6:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 3},  // setup
		8:  {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 8},  // loop
		10: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino", 22}, // vino
		12: {"/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino", 1},     // secondFunction
	}, sourceMap.cppPreprocessed)
	dumpCppToInoMap(sourceMap.toIno)
	dumpInoToCppMap(sourceMap.toCpp)
	dumpCppToInoMap(sourceMap.cppPreprocessed)
	dumpInoToCppMap(sourceMap.inoPreprocessed)
	sourceMap.deleteCppLine(21)
	fmt.Println("\nRemoved line 21")
	dumpCppToInoMap(sourceMap.toIno)
	dumpInoToCppMap(sourceMap.toCpp)
	dumpCppToInoMap(sourceMap.cppPreprocessed)
	dumpInoToCppMap(sourceMap.inoPreprocessed)
}

// func TestUpdateSourceMaps1(t *testing.T) {
// 	sourceMap := &InoMapper{
// 		toCpp: map[int]int{
// 			0: 1,
// 			1: 2,
// 			2: 0,
// 			3: 5,
// 			4: 3,
// 			5: 4,
// 		},
// 		toIno: make(map[int]int),
// 	}
// 	for s, t := range sourceMap.toCpp {
// 		sourceMap.toIno[t] = s
// 	}
// 	sourceMap.Update(0, 1, "foo\nbar\nbaz")
// 	if !reflect.DeepEqual(sourceMap.toCpp, map[int]int{
// 		0: 1,
// 		1: 2,
// 		2: 3,
// 		3: 4,
// 		4: 0,
// 		5: 7,
// 		6: 5,
// 		7: 6},
// 	) {
// 		t.Error(sourceMap.toCpp)
// 	}
// 	if !reflect.DeepEqual(sourceMap.toIno, map[int]int{
// 		0: 4,
// 		1: 0,
// 		2: 1,
// 		3: 2,
// 		4: 3,
// 		5: 6,
// 		6: 7,
// 		7: 5},
// 	) {
// 		t.Error(sourceMap.toIno)
// 	}
// }

// func TestUpdateSourceMaps2(t *testing.T) {
// 	sourceMap := &InoMapper{
// 		toCpp: map[int]int{
// 			0: 1,
// 			1: 2,
// 			2: 0,
// 			3: 5,
// 			4: 3,
// 			5: 4},
// 		toIno: make(map[int]int),
// 	}
// 	for s, t := range sourceMap.toCpp {
// 		sourceMap.toIno[t] = s
// 	}
// 	sourceMap.Update(2, 1, "foo")
// 	if !reflect.DeepEqual(sourceMap.toCpp, map[int]int{
// 		0: 0,
// 		1: 1,
// 		2: 2,
// 		3: 3},
// 	) {
// 		t.Error(sourceMap.toCpp)
// 	}
// 	if !reflect.DeepEqual(sourceMap.toIno, map[int]int{
// 		0: 0,
// 		1: 1,
// 		2: 2,
// 		3: 3},
// 	) {
// 		t.Error(sourceMap.toIno)
// 	}
// }
