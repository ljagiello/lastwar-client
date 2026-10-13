//go:build !linux && !darwin

package shumei

import (
	"errors"
	"time"
)

// uptime isn't implemented here; hostUptime falls back to the process's age.
func uptime() (time.Duration, error) {
	return 0, errors.New("shumei: reading the uptime is not supported on this OS")
}
