//go:build !darwin

package main

import (
	"os"
	"time"
)

// birthtime on platforms whose stat carries no birth time: the observation
// omits created rather than guessing (see birthtime_darwin.go).
func birthtime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
