package game

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Wounded compensation (MASTER.md §4 #2): after a newbie-protected city is attacked, init
// compensateArr[] {uuid, reward[], time (ms), targetInfo, scale} lists troop refunds, claimed one at
// a time with `user.get.defend.wall.compensate {uuid: Long}` (UserGetDefendWallCompensateMessage.lua:6).
//
// time + newplayer_death_return.k2 seconds is when an entry becomes claimable, not a deadline: the
// client computes endTime = time + k2*1000 (WoundedCompensateData.lua:4, :21-23) and shows the
// Collect button only once now > endTime, with "Collectable in {0}" (locale 311161) before that
// (UIWoundedCompensateView.lua:108-123). MASTER.md §4.2 #2 reads it the other way round. No client
// code drops an unclaimed entry; whether the server ever expires one is unknown. Static-only: not
// sent live yet.
const (
	woundedCompensationCmd = "user.get.defend.wall.compensate"

	// woundedCompensationWait is newplayer_death_return.k2 (3600 s) from table item (1.0.364;
	// identical in live 39516). DataConfig overwrites init.dataConfig rows with the local item
	// table (DataConfig.lua:47-68, 34-44), so the table value is the one the client uses.
	woundedCompensationWait = 3600 * time.Second
)

func init() {
	registerFeature(Feature{
		Name:    "wounded-compensation",
		Summary: "claim the newbie-protection troop refunds (compensateArr) once each is claimable",
		Run:     runWoundedCompensation,
	})
	session.RegisterBenignErrorCode("120289", woundedCompensationCmd)
}

// woundedCompensationDue returns the uuids of compensateArr entries whose wait has passed.
func woundedCompensationDue(in *Init, now time.Time) []int64 {
	var out []int64
	for _, e := range in.Objects("compensateArr") {
		uuid, ok := claimInt(e, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		at, ok := claimInt(e, "time")
		if !ok || now.UnixMilli() <= at+woundedCompensationWait.Milliseconds() {
			continue
		}
		out = append(out, uuid)
	}
	return out
}

func runWoundedCompensation(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("wounded-compensation: no init push; skipping")
		return nil
	}
	due := woundedCompensationDue(in, claimNow(in))
	if len(due) == 0 {
		slog.Info("wounded-compensation: nothing claimable", "entries", len(in.Objects("compensateArr")))
		return nil
	}
	return errors.Join(claimEach(due, func(uuid int64) error {
		params := sfs.NewSFSObject()
		params.PutLong("uuid", uuid)
		_, err := claimAndLog(conn, fmt.Sprintf("wounded compensation %d", uuid), woundedCompensationCmd, params)
		return err
	})...)
}
