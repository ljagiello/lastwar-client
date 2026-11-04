package game

import (
	"errors"
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Ask the alliance for help on your own running timers (MASTER.md §4 #13). OPT-IN: a help request
// is visible to every alliance member, so this never runs unless enabled.
//
// `al.call.help {uuid: Long, type: Int, qType: Int, itemId: UtfString}` (AllianceCallHelpMessage.lua:
// 4-10) is free and is what the city help bubbles send (BuildBubbleManager.lua:1327-1365,
// 3033-3041, 3434-3442, 3854-3861). AllianceHelpType is 0 building, 1 queue, 2 repair
// (EnumType.lua:5265-5270); a building's help field is a bitfield, 1 upgrade helped, 2 repair
// helped (:9745-9750). A building's bubbles run only while it is not folded (state != 2,
// BuildingStateType, EnumType.lua:3299-3305): the upgrade bubble while dStT <= 0 and the repair
// bubble while dStT > 0 (BuildBubbleManager.lua:935, 3417; WorldBuildBubbleManager.lua:121, 237).
// Requests, each sent once per timer (the server then sets help/isHelped):
//   - upgrade: {building uuid, 0, 0, ""} while lv > 0 && dStT <= 0 && now < uT && help ∈ {0, 2}
//     (:1327-1335);
//   - repair: {building uuid, 2, 0, ""} while lv > 0 && dStT > 0 && now < dEndT && help ∈ {0, 1}
//     and the upgrade has not finished, !(uT > 0 && now >= uT) (IsUpgradeFinish, :3434-3442,
//     BuildingDate.lua:453-456);
//   - research: {queue uuid, 1, 6, queue itemObj.itemId} while the type-6 queue runs and
//     isHelped == 0;
//   - healing: {queue uuid, 1, 3 or 117, itemId} while the hospital or rebirth queue runs and
//     isHelped == 0. The client also limits hospital requests to medical_assist_daily_limit.k1 per
//     day, which is 9999 in the item table, so isHelped is the effective gate.
//
// Static-only: not sent live yet.
const (
	callHelpCmd    = "al.call.help"
	callHelpFoldUp = 2 // BuildingStateType.FoldUp; init building_new state, missing = 0 (BuildingDate.lua:71-75)
)

func init() {
	registerFeature(Feature{
		Name:    "alliance-call-help",
		Summary: "OPT-IN: ask the alliance for help on your own running upgrades, repairs, research and healing (visible to alliance members)",
		Run:     runAllianceCallHelp,
	})
	session.RegisterBenignErrorCode("120289", callHelpCmd)
}

type callHelpRequest struct {
	uuid, kind, qType int64
	itemID, what      string
}

func runAllianceCallHelp(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("alliance-call-help: no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info("alliance-call-help: not in an alliance; skipping")
		return nil
	}
	now := claimNow(in).UnixMilli()
	var reqs []callHelpRequest
	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		lv, _ := claimInt(b, "lv")
		if !ok || uuid == 0 || lv <= 0 {
			continue
		}
		if state, _ := claimInt(b, "state"); state == callHelpFoldUp {
			continue
		}
		help, _ := claimInt(b, "help")
		uT, _ := claimInt(b, "uT")
		upgradeDone := uT > 0 && now >= uT
		if dStT, _ := claimInt(b, "dStT"); dStT <= 0 {
			if uT > 0 && !upgradeDone && (help == 0 || help == 2) {
				reqs = append(reqs, callHelpRequest{uuid, 0, 0, "", "upgrade"})
			}
		} else if dEndT, _ := claimInt(b, "dEndT"); now < dEndT && !upgradeDone && (help == 0 || help == 1) {
			reqs = append(reqs, callHelpRequest{uuid, 2, 0, "", "repair"})
		}
	}
	for _, q := range finishedTimersQueues(in) {
		if !q.Working(now) {
			continue
		}
		switch q.Type {
		case finishedTimersQueueResearch, finishedTimersQueueHospital, finishedTimersQueueRebirth:
		default:
			continue
		}
		if helped, _ := claimInt(q.Raw, "isHelped"); helped != 0 {
			continue
		}
		var itemID string
		if v, ok := q.Raw.Get("itemObj"); ok {
			if obj, ok := v.Val.(*sfs.SFSObject); ok {
				itemID, _ = claimString(obj, "itemId")
			}
		}
		reqs = append(reqs, callHelpRequest{q.UUID, 1, q.Type, itemID, fmt.Sprintf("queue type %d", q.Type)})
	}
	if len(reqs) == 0 {
		slog.Info("alliance-call-help: nothing running that still needs a help request")
		return nil
	}
	return errors.Join(claimEach(reqs, func(r callHelpRequest) error {
		p := sfs.NewSFSObject()
		p.PutLong("uuid", r.uuid)
		p.PutInt("type", int32(r.kind))
		p.PutInt("qType", int32(r.qType))
		p.PutUtfString("itemId", r.itemID)
		_, err := session.SendAndWait(conn, fmt.Sprintf("alliance help request (%s %d)", r.what, r.uuid), callHelpCmd, p)
		return err
	})...)
}
