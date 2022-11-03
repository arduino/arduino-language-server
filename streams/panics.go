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

package streams

import (
	"fmt"
	"log"
	"runtime/debug"
)

// CatchAndLogPanic will recover a panic, log it on standard logger, and rethrow it
// to continue stack unwinding.
func CatchAndLogPanic() {
	if r := recover(); r != nil {
		reason := fmt.Sprintf("%v", r)
		log.Println(fmt.Sprintf("Panic: %s\n\n%s", reason, string(debug.Stack())))
		panic(reason)
	}
}
