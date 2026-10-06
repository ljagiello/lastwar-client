package game

import (
	"fmt"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season output collectors (MASTER.md §4 #19), static-only, sent only while the account is in a
// season (evSeasonID).
//
//   - Mummies (S3-S6): lw.season.mummy.get {} while the season's mummy yard is built and init
//     mummyWaitRecInfo[].armyNum sums above 0 (BuildBubbleManager.lua:3218-3266, 4440-4448;
//     SeasonMummyDataManager.lua:29-42). It turns waiting fallen troops into mummy soldiers; the
//     server enforces the stock cap (effect 94064), which init alone can't give.
//   - City output (S5, S6): lw.season.alliance.city.occupy.info {} -> rewardInfo[{leftNum}];
//     batch.get.city.output {} when any leftNum > 0 (UILWSeasonCityOccupyListS6View.lua:214-221).
//   - Stronghold output (S2-S6): lw.season.city.stronghold.occupy.info {} -> rewardInfo[{id,
//     serverId, leftNum}]; get.stronghold.output {serverId: Int, strongholdId: Int} per entry with
//     leftNum > 0, the S5/S6 per-item path (S5Item.lua:44-50). Its handler treats
//     season_s2_tips_004 as "refresh the list" (CollectStrongholdResourceMessage.lua:12-15).
//   - City attachments (S1): city.attachment.list {} -> rewardList[{leftNum}];
//     city.attachment.rec.all.output {} when they sum above 0 and the SeasonFarmer activity (239)
//     runs (SeasonFarmerManager.lua:67-117).
//   - Faction production (S6): camp.product.view {} -> campProductArr[{serverId, cityId, num}],
//     userCampCityRewardArr[{cityId, receiveNum}]; get.camp.product.reward {serverCityArr:
//     [{serverId: Int, cityId: Int}], isShake: Bool false} for every city with num > receiveNum,
//     while the CampScience activity (392) runs (CampProduceDataManager.lua:21-55, 278-306).
//
// The alliance reads are sent only with init user.allianceId set, as OnEnterGame does
// (SeasonDataManager.lua:173-200). Not sent: user.collect.desert.res (legacy desert seasons; the
// pending amounts are computed client-side from effects) and get.alliance.build.output.reward (its
// buildId comes from an alliance-mine template mapping that needs a live capture).
const (
	seasonMummyCmd          = "lw.season.mummy.get"
	seasonCityInfoCmd       = "lw.season.alliance.city.occupy.info"
	seasonCityOutputCmd     = "batch.get.city.output"
	seasonStrongholdInfoCmd = "lw.season.city.stronghold.occupy.info"
	seasonStrongholdCmd     = "get.stronghold.output"
	seasonAttachInfoCmd     = "city.attachment.list"
	seasonAttachOutputCmd   = "city.attachment.rec.all.output"
	seasonCampInfoCmd       = "camp.product.view"
	seasonCampRewardCmd     = "get.camp.product.reward"
	seasonStrongholdRefresh = "season_s2_tips_004"
)

// seasonMummyYards maps a season to its mummy-yard building (SeasonUtil.lua:456-480).
var seasonMummyYards = map[int64]int32{3: 787000, 4: 806000, 5: 831000, 6: 851000}

func init() {
	registerFeature(Feature{
		Name:    "season-output",
		Summary: "in season: collect mummies, alliance city/stronghold output, S1 attachments, S6 faction production; static-only",
		Run:     runSeasonOutput,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, seasonMummyCmd, seasonCityOutputCmd, seasonStrongholdCmd,
		seasonAttachOutputCmd, seasonCampRewardCmd)
	session.RegisterBenignErrorCode(seasonStrongholdRefresh, seasonStrongholdCmd)
}

func runSeasonOutput(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	season := evSeasonID(in)
	if season <= 0 {
		return nil
	}
	b := &evBatch{conn: conn}
	s := Activities(conn, in)
	if yard, ok := seasonMummyYards[season]; ok && evHasBuilding(in, yard) {
		var waiting int64
		for _, w := range in.Objects("mummyWaitRecInfo") {
			n, _ := evNum(w, "armyNum")
			waiting += n
		}
		if waiting > 0 {
			b.send("season mummies", seasonMummyCmd, sfs.NewSFSObject())
		}
	}
	inAlliance := evString(in.Object("user"), "allianceId") != ""
	if inAlliance && (season == 5 || season == 6) && !b.dead {
		if msg := b.send("season city output info", seasonCityInfoCmd, sfs.NewSFSObject()); msg != nil {
			for _, r := range evObjects(msg.Params, "rewardInfo") {
				if n, _ := evNum(r, "leftNum"); n > 0 {
					b.send("season city output", seasonCityOutputCmd, sfs.NewSFSObject())
					break
				}
			}
		}
	}
	if inAlliance && season >= 2 && !b.dead {
		if msg := b.send("season stronghold info", seasonStrongholdInfoCmd, sfs.NewSFSObject()); msg != nil {
			for _, r := range evObjects(msg.Params, "rewardInfo") {
				left, _ := evNum(r, "leftNum")
				server, ok1 := evNum(r, "serverId")
				id, ok2 := evNum(r, "id")
				if left <= 0 || !ok1 || !ok2 || b.dead {
					continue
				}
				p := sfs.NewSFSObject()
				p.PutInt("serverId", int32(server))
				p.PutInt("strongholdId", int32(id))
				b.send(fmt.Sprintf("season stronghold %d output", id), seasonStrongholdCmd, p)
			}
		}
	}
	if inAlliance && season == 1 && len(s.Open(actTypeSeasonFarmer)) > 0 && !b.dead {
		if msg := b.send("season attachment info", seasonAttachInfoCmd, sfs.NewSFSObject()); msg != nil {
			var left int64
			for _, r := range evObjects(msg.Params, "rewardList") {
				n, _ := evNum(r, "leftNum")
				left += n
			}
			if left > 0 {
				b.send("season attachment output", seasonAttachOutputCmd, sfs.NewSFSObject())
			}
		}
	}
	if season == 6 && len(s.Open(actTypeCampScience)) > 0 && !b.dead {
		claimCampProduction(b)
	}
	return b.err()
}

func claimCampProduction(b *evBatch) {
	msg := b.send("faction production info", seasonCampInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	received := map[int64]int64{}
	for _, r := range evObjects(msg.Params, "userCampCityRewardArr") {
		city, _ := evNum(r, "cityId")
		n, _ := evNum(r, "receiveNum")
		received[city] = n
	}
	cities := sfs.NewSFSArray()
	count := 0
	for _, c := range evObjects(msg.Params, "campProductArr") {
		city, ok1 := evNum(c, "cityId")
		server, ok2 := evNum(c, "serverId")
		num, _ := evNum(c, "num")
		if !ok1 || !ok2 || num-received[city] <= 0 {
			continue
		}
		e := sfs.NewSFSObject()
		e.PutInt("serverId", int32(server))
		e.PutInt("cityId", int32(city))
		cities.AddSFSObject(e)
		count++
	}
	if count == 0 {
		return
	}
	p := sfs.NewSFSObject()
	p.PutBool("isShake", false)
	p.PutSFSArray("serverCityArr", cities)
	b.send("faction production", seasonCampRewardCmd, p)
}
