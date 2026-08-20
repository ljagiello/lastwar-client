package sfs

// fakeTimeoutNetError is a net.Error fake reporting Timeout()==true, used by packet_oom_test.go to
// exercise ReadFrameField's mid-frame deadline handling. Mirrors the identical helper in the game
// package's conn_wait_test.go (they can't share it across the package boundary).
type fakeTimeoutNetError struct{ msg string }

func (e fakeTimeoutNetError) Error() string { return e.msg }
func (fakeTimeoutNetError) Timeout() bool   { return true }
func (fakeTimeoutNetError) Temporary() bool { return true }
