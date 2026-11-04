package game

import (
	"sync/atomic"
	"time"

	"lastwar-client/internal/session"
)

// The game time is the server's clock as the client keeps it, the one every client time gate and
// cmdBaseTime reads (UITimeManager:GetServerTime, UITimeManager.lua:48-55). It comes from the
// session's pongs (session.GameConn.ServerNow, MASTER B49). Before the first pong, or with no
// session, it is the host clock. Socket deadlines and the tripwire record's timestamp stay on the
// host clock: they are about this process, not the game.

// clockConn is the session whose server clock gameNow reads. RunFeatures and Collect set it.
var clockConn atomic.Pointer[session.GameConn]

// useServerClock makes gameNow, and so claimNow and evNow, read conn's server clock.
func useServerClock(conn *session.GameConn) {
	if conn != nil {
		clockConn.Store(conn)
	}
}

// gameNow is the game time of the session useServerClock last set.
func gameNow() time.Time {
	return connNow(clockConn.Load())
}

// connNow is conn's server time, or the host time before its first pong or without a conn.
func connNow(conn *session.GameConn) time.Time {
	if conn == nil {
		return time.Now()
	}
	now, _ := conn.ServerNow()
	return now
}
