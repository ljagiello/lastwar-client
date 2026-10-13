package shumei

import (
	"net"
	"time"
)

// processStart stands in for the boot time on a host whose uptime can't be
// read: a device that booted when this process started.
var processStart = time.Now()

// hostUptime returns how long ago the host booted, suspend included, like
// Android's elapsedRealtime(). Without a readable uptime it returns the time
// since this process started.
func hostUptime() time.Duration {
	if d, err := uptime(); err == nil && d > 0 {
		return d
	}
	return max(time.Since(processStart), 0)
}

// hostIP returns the IPv4 address the host's default route leaves from, or ""
// without one. Dialing UDP only picks the route and sends nothing; 192.0.2.1
// is TEST-NET-1 (RFC 5737).
func hostIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer func() { _ = c.Close() }()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}
