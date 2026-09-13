package xfer

import (
	"os"
	"strings"
)

// isTerminal is a dependency-free TTY probe: a character device plus a usable
// TERM value is treated as an interactive terminal. Redirecting to /dev/null
// or a pipe is therefore (correctly) not a terminal for our purposes.
func isTerminal(f *os.File) bool {
	if term := os.Getenv("TERM"); term == "" || term == "dumb" {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	if strings.HasSuffix(fi.Name(), string(os.PathSeparator)+"null") {
		return false
	}
	return true
}
