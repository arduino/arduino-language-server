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
