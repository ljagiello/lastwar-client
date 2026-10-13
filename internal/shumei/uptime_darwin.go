//go:build darwin

package shumei

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"
)

// uptime is the time since kern.boottime, a struct timeval: int64 seconds,
// then int32 microseconds, little-endian on both macOS architectures.
// syscall.Sysctl drops one trailing NUL byte, which can only come from the
// timeval's padding.
func uptime() (time.Duration, error) {
	s, err := syscall.Sysctl("kern.boottime")
	if err != nil {
		return 0, err
	}
	b := []byte(s)
	if len(b) < 12 {
		return 0, fmt.Errorf("shumei: kern.boottime is %d bytes, want a timeval", len(b))
	}
	sec := int64(binary.LittleEndian.Uint64(b[:8]))
	usec := int64(binary.LittleEndian.Uint32(b[8:12]))
	return time.Since(time.Unix(sec, usec*int64(time.Microsecond))), nil
}
