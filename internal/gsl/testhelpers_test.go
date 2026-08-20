package gsl

import "strconv"

// flexPort builds a FlexString from an int port, for constructing LoginServerInfo fixtures in
// this package's tests. Mirrors the game package's own copy (test helpers can't cross packages).
func flexPort(n int) FlexString { return FlexString(strconv.Itoa(n)) }
