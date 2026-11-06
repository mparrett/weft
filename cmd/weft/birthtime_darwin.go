//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

// birthtime reports the file's creation time where the platform records one.
//
// Darwin's stat carries a birth time; Linux needs statx, which the stdlib does
// not expose, and reaching for x/sys to get it is a dependency BUILD.md §1.1
// prices against every invocation. So on other platforms the observation
// simply omits created — EVENTS.md §3.8 makes absence mean "not reported",
// which is the honest state.
func birthtime(fi os.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec), true
}
