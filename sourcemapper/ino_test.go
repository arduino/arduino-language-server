package sourcemapper

import (
	"fmt"
	"testing"

	"github.com/arduino/go-paths-helper"
	"github.com/stretchr/testify/require"
)

func TestCreateSourceMaps(t *testing.T) {
	input := `#include <Arduino.h>
#line 1 "/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino"
#line 1 "/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino"

#line 2 "/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino"
void setup();
#line 7 "/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino"
void loop();
#line 2 "/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino"
void setup() {
	// put your setup code here, to run once:
	
}

void loop() {
	// put your main code here, to run repeatedly:
	
}
`
	sourceMap := CreateInoMapper([]byte(input))
	sketchJuly2a := paths.New("/home/megabug/Workspace/arduino-language-server/handler/sourcemapper/sketch_july2a.ino").Canonical().String()
	require.EqualValues(t, map[InoLine]int{
		{sketchJuly2a, 0}:  3,
		{sketchJuly2a, 1}:  9,
		{sketchJuly2a, 2}:  10,
		{sketchJuly2a, 3}:  11,
		{sketchJuly2a, 4}:  12,
		{sketchJuly2a, 5}:  13,
		{sketchJuly2a, 6}:  14,
		{sketchJuly2a, 7}:  15,
		{sketchJuly2a, 8}:  16,
		{sketchJuly2a, 9}:  17,
		{sketchJuly2a, 10}: 18,
	}, sourceMap.toCpp)
	require.EqualValues(t, map[int]InoLine{
		0:  NotIno,
		1:  NotIno,
		2:  NotIno,
		3:  {sketchJuly2a, 0},
		4:  NotIno,
		5:  {sketchJuly2a, 1}, // setup
		6:  NotIno,
		7:  {sketchJuly2a, 6}, // loop
		8:  NotIno,
		9:  {sketchJuly2a, 1},
		10: {sketchJuly2a, 2},
		11: {sketchJuly2a, 3},
		12: {sketchJuly2a, 4},
		13: {sketchJuly2a, 5},
		14: {sketchJuly2a, 6},
		15: {sketchJuly2a, 7},
		16: {sketchJuly2a, 8},
		17: {sketchJuly2a, 9},
		18: {sketchJuly2a, 10},
	}, sourceMap.toIno)
	require.EqualValues(t, map[int]InoLine{
		5: {sketchJuly2a, 1}, // setup
		7: {sketchJuly2a, 6}, // loop
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
	ProvaSpazio := paths.New("/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/Prova_Spazio.ino").Canonical().String()
	SecondTab := paths.New("/home/megabug/Workspace/sketchbook-cores-beta/Prova_Spazio/SecondTab.ino").Canonical().String()
	sourceMap := CreateInoMapper([]byte(input))
	require.EqualValues(t, sourceMap.toCpp, map[InoLine]int{
		{ProvaSpazio, 0}:  2,
		{ProvaSpazio, 1}:  3,
		{ProvaSpazio, 2}:  4,
		{ProvaSpazio, 3}:  14,
		{ProvaSpazio, 4}:  15,
		{ProvaSpazio, 5}:  16,
		{ProvaSpazio, 6}:  17,
		{ProvaSpazio, 7}:  18,
		{ProvaSpazio, 8}:  19,
		{ProvaSpazio, 9}:  20,
		{ProvaSpazio, 10}: 21,
		{ProvaSpazio, 11}: 22,
		{ProvaSpazio, 12}: 23,
		{ProvaSpazio, 13}: 24,
		{ProvaSpazio, 14}: 25,
		{ProvaSpazio, 15}: 26,
		{ProvaSpazio, 16}: 27,
		{ProvaSpazio, 17}: 28,
		{ProvaSpazio, 18}: 29,
		{ProvaSpazio, 19}: 30,
		{ProvaSpazio, 20}: 31,
		{ProvaSpazio, 21}: 32,
		{ProvaSpazio, 22}: 33,
		{ProvaSpazio, 23}: 34,
		{ProvaSpazio, 24}: 35,
		{SecondTab, 0}:    37,
		{SecondTab, 1}:    38,
		{SecondTab, 2}:    39,
		{SecondTab, 3}:    40,
		{SecondTab, 4}:    41,
	})
	require.EqualValues(t, sourceMap.toIno, map[int]InoLine{
		0:  NotIno,
		1:  NotIno,
		2:  {ProvaSpazio, 0},
		3:  {ProvaSpazio, 1},
		4:  {ProvaSpazio, 2},
		5:  NotIno,
		6:  {ProvaSpazio, 3}, // setup
		7:  NotIno,
		8:  {ProvaSpazio, 8}, // loop
		9:  NotIno,
		10: {ProvaSpazio, 22}, // vino
		11: NotIno,
		12: {SecondTab, 1}, // secondFunction
		13: NotIno,
		14: {ProvaSpazio, 3},
		15: {ProvaSpazio, 4},
		16: {ProvaSpazio, 5},
		17: {ProvaSpazio, 6},
		18: {ProvaSpazio, 7},
		19: {ProvaSpazio, 8},
		20: {ProvaSpazio, 9},
		21: {ProvaSpazio, 10},
		22: {ProvaSpazio, 11},
		23: {ProvaSpazio, 12},
		24: {ProvaSpazio, 13},
		25: {ProvaSpazio, 14},
		26: {ProvaSpazio, 15},
		27: {ProvaSpazio, 16},
		28: {ProvaSpazio, 17},
		29: {ProvaSpazio, 18},
		30: {ProvaSpazio, 19},
		31: {ProvaSpazio, 20},
		32: {ProvaSpazio, 21},
		33: {ProvaSpazio, 22},
		34: {ProvaSpazio, 23},
		35: {ProvaSpazio, 24},
		36: {"/not-ino", 0},
		37: {SecondTab, 0},
		38: {SecondTab, 1},
		39: {SecondTab, 2},
		40: {SecondTab, 3},
		41: {SecondTab, 4},
	})
	require.EqualValues(t, map[int]InoLine{
		6:  {ProvaSpazio, 3},  // setup
		8:  {ProvaSpazio, 8},  // loop
		10: {ProvaSpazio, 22}, // vino
		12: {SecondTab, 1},    // secondFunction
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
