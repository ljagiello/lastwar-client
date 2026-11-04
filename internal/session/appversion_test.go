package session

import "testing"

func TestGameConnAppVersion(t *testing.T) {
	var none *GameConn
	if got := none.AppVersion(); got != "" {
		t.Errorf("nil conn AppVersion() = %q, want \"\"", got)
	}
	c, _ := NewPipeGameConnPair(t)
	if got := c.AppVersion(); got != "" {
		t.Errorf("AppVersion() before any Login = %q, want \"\"", got)
	}
	c.SetAppVersion("1.0.344")
	if got := c.AppVersion(); got != "1.0.344" {
		t.Errorf("AppVersion() = %q, want 1.0.344", got)
	}
}
