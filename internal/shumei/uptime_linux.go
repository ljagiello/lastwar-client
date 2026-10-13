//go:build linux

package shumei

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// uptime reads /proc/uptime's first field: seconds since boot, suspend
// included (CLOCK_BOOTTIME, the clock Android's elapsedRealtime() reads).
func uptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	secs, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return 0, fmt.Errorf("shumei: parse /proc/uptime %q: %w", field, err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
