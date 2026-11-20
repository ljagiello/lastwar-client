package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance firework chests (FIREWORKS.md), static-only. When an ally sets off a firework at a base,
// that HQ's world tile carries the chests it dropped (BuildInfo.fireWorksGiftList, field 54), and
// the real client claims one when the player taps the box over the base
// (FireworkWorldBuildBubble.lua:117-131). No chat is involved. Over SFS:
//
//  1. al.rank {allianceId: Utf} lists the members with their HQ pointId (AlRankMessage.lua:3-7;
//     AllianceMemberInfo.lua:67-94). The client sends it with init alliance.uid
//     (TryInitMemberList, AllianceMemberDataManager.lua:59-73; alliance.uid comes from init via
//     AllianceBaseDataManager.lua:127-140) and treats the reply as its own alliance when it matches
//     user.allianceId (PlayerInfo.lua:567-568). This reads alliance.uid, else user.allianceId.
//  2. world.get.block for the blocks around those HQs, built like the world scene's viewport
//     request (WorldGetBlockMessage.CSSetData, CS:84579-84616; UpdateLWAoi_Normal,
//     CS:179758-179842), one rectangle per request with a pause between requests. See
//     fwPlanBlocksAround for the block arithmetic.
//  3. points[] and alInfos[] in the reply are protobuf WorldPointInfo blobs (ParseWorldGetBlock,
//     CS:180719), decoded by worldpoint_proto.go.
//  4. fireworks only: get.fireworks.gift {uuid: Long, ownerUid: Utf, type: Int} per eligible chest
//     (GetFireworksGiftMessage.lua:3-8), type being firework.type for the chest's configId. The
//     claim is free; the launcher spent the firework.
//
// A chest is eligible when it was sent before now and less than 120 minutes ago (DataConfig
// fireworks.k2; the tile applies it to type 1 only, but the chat card to every chest, and the
// server answers firework_tips_1014 for expired chests), holds fewer claims than its max, sits on
// a building with an owner, and, for type 0, the owner is a current alliance member, for type 1,
// the building is of this alliance (GetCurAvailableBox, FireworkWorldBuildBubble.lua:281-296: the
// member lookup is in the type-0 branch only; MASTER B36). The tile shows no claimers; the
// client's own "got" list is local, so a chest claimed on another device comes back as
// firework_tips_1016, which is benign.
//
// The window stays on type 0 on purpose (1.0.367 review). The client keeps every chest it claimed,
// or got 1016 for, in a 24 h PlayerPrefs list and skips those (LWFireworkGiftManager.lua:7-11,
// 41-63; GetFireworksGiftMessage.lua:14-18; FireworkWorldBuildBubble.lua:281). Go keeps no such
// list, so the window is what stops a 3-hourly run from re-claiming a chest an earlier run already
// claimed. And 120 is the table's k2, not init's: DataConfig:InitFromNet re-applies table item over
// init dataConfig for every row the table has, fireworks among them (DataConfig.lua:36-46, 52-74).
//
// Not done: the auto-like of the launcher the claim window plays for configs with
// firework.auto_like (UIFireworkGiftGotShowItemRender.lua:104-120; visible to other players),
// setting off fireworks, find.fireworks.gift.world.point (camera only), and the season big map
// (3000 tiles, UpdateLWAoi_Big3000), whose point ids exceed 1,000,000 and are skipped.
//
// Unverified live: whether the server answers world.get.block, with field 54, for a session that
// never entered the world scene. A firework claim scores in no Alliance Duel score row
// (TB:score has no firework type or value), so neither feature declares Duel.
//
// Steps 1-3 are allianceTileScan, run once per run (sharedAllianceTileScan, MASTER B34) for every
// feature that reads the member HQ tiles: fireworks, alliance-treasures and the opt-in
// base-bubbles. The one scan reads every block within atBlockRadius
// of each HQ, so al.rank and clearUuidSet go out once (the client sends clearUuidSet only on its
// first request after entering the world, CS:179977-179982).

const (
	fireworksRankCmd  = "al.rank"
	fireworksBlockCmd = "world.get.block"
	fireworksGiftCmd  = "get.fireworks.gift"

	fwMapTiles            = 1000                     // the normal map (UpdateLWAoi_Normal, A-CS:179042)
	fwBlockSize           = 10                       // LWAoiBlockSizeArray[viewLvl 0] (ConstDefine.lua:290-294)
	fwBlockCount          = fwMapTiles / fwBlockSize // RecalculateBlockArg (A-CS:182711-182716)
	fwMaxBlocksPerRequest = 160                      // per world.get.block (A-CS:179114-179128)

	fwBoxLifetime     = 120 * time.Minute // DataConfig fireworks.k2
	fwMaxClaimsPerRun = 20
	fwDailyCapCode    = "firework_tips_1009" // "You've collected over 100 Fireworks Chests today"
	fwSlowDownCode    = "firework_tips_1017" // "Slow down, Commander. Please try again in a moment"
)

// fireworksBenignCodes are the claim's "nothing to claim" answers (locale en.tsv:44590-44592).
var fireworksBenignCodes = map[string]string{
	"firework_tips_1014": "expired",
	"firework_tips_1015": "all claimed",
	"firework_tips_1016": "already claimed by you",
}

// fireworksClaimPace is the gap between claims, about a person tapping one box after another.
var fireworksClaimPace = time.Second

// fwSleep is time.Sleep; tests replace it.
var fwSleep = time.Sleep

// fwBlockPace is the gap between world.get.block requests, about a camera moved from one cluster
// of bases to the next; fwBlockSleep is time.Sleep, replaced in tests.
var (
	fwBlockPace  = 500 * time.Millisecond
	fwBlockSleep = time.Sleep
)

func init() {
	registerFeature(Feature{
		Name: "fireworks-scan",
		Summary: "read-only: read the alliance members' HQ tiles (al.rank, world.get.block) and log every firework " +
			"chest on them and whether it is claimable; static-only",
		Run: runFireworksScan,
	})
	registerFeature(Feature{
		Name: "fireworks",
		Summary: "OPT-IN: claim the firework chests allies set off on member HQs (get.fireworks.gift, free); " +
			"the server may show the claim over that base and post a double-reward line in alliance chat; static-only",
		Run: runFireworks,
	})
	for code := range fireworksBenignCodes {
		session.RegisterBenignErrorCode(code, fireworksGiftCmd)
	}
}

// fwType is firework.type for configId: 1 for the S0 alliance-boss chests 671001-671005, 0 for
// every other row and for unknown ids (GetTableData default 0; firework.json, 1.0.364).
func fwType(configID int32) int32 {
	if configID >= 671001 && configID <= 671005 {
		return 1
	}
	return 0
}

// Block arithmetic on the normal 1000x1000 map at viewLvl 0 (UpdateLWAoi_Normal,
// A-CS:179042-179131, and its helpers):
//
//	tile of a point id p:   x = (p-1) % 1000, y = (p-1) / 1000         IndexToTilePos, A-CS:115017-115025
//	point id of a tile:     x + y*1000 + 1, after clamping to 0..999    TilePosToIndex + ClampTilePos, A-CS:115032-115039, 115076-115095
//	block of a tile:        (x/10, y/10)                                PositionToAoiBlock, A-CS:182718-182723
//	block index:            by*100 + bx                                 AoiBlockToIndex, A-CS:182725-182728
//	leftBottom / rightTop:  the tiles (bx1*10, by1*10) and (bx2*10+10, by2*10+10)
//
// e.g. point 347513 is tile (512, 347), block (51, 34), index 3451, leftBottom 340511 and
// rightTop 350521.

// fwTilePos is IndexToTilePos; ok is false outside the map.
func fwTilePos(pointID int64) (x, y int, ok bool) {
	if pointID < 1 || pointID > fwMapTiles*fwMapTiles {
		return 0, 0, false
	}
	p := int(pointID - 1)
	return p % fwMapTiles, p / fwMapTiles, true
}

// fwTileIndex is TilePosToIndex after ClampTilePos.
func fwTileIndex(x, y int) int32 {
	x, y = min(max(x, 0), fwMapTiles-1), min(max(y, 0), fwMapTiles-1)
	return int32(x + y*fwMapTiles + 1)
}

// fwBlockIndex is the block index of a tile.
func fwBlockIndex(x, y int) int {
	return (y/fwBlockSize)*fwBlockCount + x/fwBlockSize
}

// fwBlockRequest is one world.get.block: the block indexes plus the rectangle around them and a
// camera tile inside it.
type fwBlockRequest struct {
	X, Y                 int32
	Index                []int32
	LeftBottom, RightTop int32
}

// fwRect is a rectangle of blocks, x0..x1 by y0..y1 inclusive.
type fwRect struct{ x0, y0, x1, y1 int }

func (r fwRect) area() int { return (r.x1 - r.x0 + 1) * (r.y1 - r.y0 + 1) }

func (r fwRect) union(o fwRect) fwRect {
	return fwRect{min(r.x0, o.x0), min(r.y0, o.y0), max(r.x1, o.x1), max(r.y1, o.y1)}
}

// fwViewWidth is the widest rectangle the planner sends, in blocks: 13 x 12 = 156 stays within the
// client's 160 blocks per request.
const fwViewWidth = 13

// fwPlanBlocks turns HQ point ids into world.get.block requests for their own blocks.
func fwPlanBlocks(points []int64) []fwBlockRequest { return fwPlanBlocksAround(points, 0) }

// fwPlanBlocksAround plans the world.get.block requests for each point's block and every block
// within radius blocks of it in each direction, clipped to the map, shaped like the client's
// camera viewports (MASTER B34; UpdateLWAoi_Normal, CS:179758-179842): one request covers one
// rectangle of contiguous blocks, its corners as leftBottom/rightTop (the tiles block*10 and
// block*10 + 10, clamped) and its centre as the camera tile x/y, with at most 160 blocks. The
// wanted blocks are grouped into clusters of adjacent blocks (8-neighbour); a cluster's bounding
// rectangle is one request, a cluster too big for one is cut into rectangles of at most
// fwViewWidth x 12 blocks, each shrunk to the wanted blocks in it, and two rectangles are merged
// while the merged one stays within 160 blocks and at most twice their summed size. Each
// request's index lists the blocks of its rectangle row by row, leaving out the blocks an
// earlier request already read, as the client sends only the blocks not yet in view
// (`if (!_curViewIndex.Contains(item)) _addViewIndex.Add(item)`). Before the 1.0.370 audit the
// sorted block list was cut into chunks of 160 whose bounding rectangle could span most of the
// map.
func fwPlanBlocksAround(points []int64, radius int) []fwBlockRequest {
	want := map[int]bool{}
	for _, p := range points {
		x, y, ok := fwTilePos(p)
		if !ok {
			continue
		}
		bx, by := x/fwBlockSize, y/fwBlockSize
		for ny := max(by-radius, 0); ny <= min(by+radius, fwBlockCount-1); ny++ {
			for nx := max(bx-radius, 0); nx <= min(bx+radius, fwBlockCount-1); nx++ {
				want[ny*fwBlockCount+nx] = true
			}
		}
	}
	var rects []fwRect
	for _, c := range fwClusters(want) {
		rects = append(rects, fwCutRect(c, want)...)
	}
	rects = fwMergeRects(rects)
	slices.SortFunc(rects, func(a, b fwRect) int { return cmp.Or(cmp.Compare(a.y0, b.y0), cmp.Compare(a.x0, b.x0)) })
	seen := map[int]bool{}
	var out []fwBlockRequest
	for _, rc := range rects {
		var r fwBlockRequest
		for by := rc.y0; by <= rc.y1; by++ {
			for bx := rc.x0; bx <= rc.x1; bx++ {
				if i := by*fwBlockCount + bx; !seen[i] {
					seen[i] = true
					r.Index = append(r.Index, int32(i))
				}
			}
		}
		if len(r.Index) == 0 {
			continue
		}
		lbX, lbY := rc.x0*fwBlockSize, rc.y0*fwBlockSize
		rtX, rtY := rc.x1*fwBlockSize+fwBlockSize, rc.y1*fwBlockSize+fwBlockSize
		r.LeftBottom, r.RightTop = fwTileIndex(lbX, lbY), fwTileIndex(rtX, rtY)
		r.X, r.Y = int32((lbX+rtX)/2), int32((lbY+rtY)/2)
		out = append(out, r)
	}
	return out
}

// fwClusters returns the bounding rectangle of each 8-connected group of blocks in want, in no
// particular order.
func fwClusters(want map[int]bool) []fwRect {
	done := map[int]bool{}
	var out []fwRect
	for _, start := range slices.Sorted(maps.Keys(want)) {
		if done[start] {
			continue
		}
		done[start] = true
		rc := fwRect{start % fwBlockCount, start / fwBlockCount, start % fwBlockCount, start / fwBlockCount}
		for stack := []int{start}; len(stack) > 0; {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			bx, by := i%fwBlockCount, i/fwBlockCount
			rc = rc.union(fwRect{bx, by, bx, by})
			for ny := max(by-1, 0); ny <= min(by+1, fwBlockCount-1); ny++ {
				for nx := max(bx-1, 0); nx <= min(bx+1, fwBlockCount-1); nx++ {
					if n := ny*fwBlockCount + nx; want[n] && !done[n] {
						done[n] = true
						stack = append(stack, n)
					}
				}
			}
		}
		out = append(out, rc)
	}
	return out
}

// fwCutRect returns rc when it fits one request, else its fwViewWidth x 12 pieces that hold a
// wanted block, each shrunk to the wanted blocks in it.
func fwCutRect(rc fwRect, want map[int]bool) []fwRect {
	if rc.area() <= fwMaxBlocksPerRequest {
		return []fwRect{rc}
	}
	w := min(rc.x1-rc.x0+1, fwViewWidth)
	h := fwMaxBlocksPerRequest / w
	var out []fwRect
	for y := rc.y0; y <= rc.y1; y += h {
		for x := rc.x0; x <= rc.x1; x += w {
			var piece fwRect
			found := false
			for by := y; by <= min(y+h-1, rc.y1); by++ {
				for bx := x; bx <= min(x+w-1, rc.x1); bx++ {
					if !want[by*fwBlockCount+bx] {
						continue
					}
					if b := (fwRect{bx, by, bx, by}); found {
						piece = piece.union(b)
					} else {
						piece, found = b, true
					}
				}
			}
			if found {
				out = append(out, piece)
			}
		}
	}
	return out
}

// fwMergeRects merges, smallest result first, any two rectangles whose union holds at most 160
// blocks and at most twice their summed size, until no pair qualifies.
func fwMergeRects(rects []fwRect) []fwRect {
	for {
		bi, bj, best := -1, -1, 0
		for i := range rects {
			for j := i + 1; j < len(rects); j++ {
				u := rects[i].union(rects[j]).area()
				if u > fwMaxBlocksPerRequest || u > 2*(rects[i].area()+rects[j].area()) {
					continue
				}
				if bi < 0 || u < best {
					bi, bj, best = i, j, u
				}
			}
		}
		if bi < 0 {
			return rects
		}
		rects[bi] = rects[bi].union(rects[bj])
		rects = slices.Delete(rects, bj, bj+1)
	}
}

// fwBlockParams is WorldGetBlockMessage.CSSetData's request for r on the own server: bigMap 0
// (normal map), worldId 0 (not in a battlefield), type 0, viewLvl 0, timeStamp as the client's
// (int) cast of server ms, which keeps the low 32 bits, and clearUuidSet 1 on the first request
// (the client's firstTimeReqAoi, which makes the server forget the points it already sent this
// session). The session adds `_id`.
func fwBlockParams(r fwBlockRequest, serverID int64, first bool, now time.Time) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutInt("bigMap", 0)
	p.PutInt("x", r.X)
	p.PutInt("y", r.Y)
	p.PutInt("serverId", int32(serverID))
	p.PutInt("worldId", 0)
	p.PutInt("type", 0)
	p.PutInt("viewLvl", 0)
	p.PutInt("timeStamp", int32(now.UnixMilli()))
	p.PutIntArray("index", r.Index)
	p.PutInt("blockSize", fwBlockSize)
	if first {
		p.PutInt("clearUuidSet", 1)
	}
	p.PutInt("leftBottom", r.LeftBottom)
	p.PutInt("rightTop", r.RightTop)
	return p
}

// fwRequest sends cmd and waits for its reply like session.SendAndWait, but logs only the
// errorCode and the reply's field names: al.rank lists every member's uid and name, the tiles carry
// owner uids, and the claim reply carries the launcher's player info (sendGiftPlayerInfo,
// UIFireworkGiftGotShowItemRender.lua:86), while SendAndWait logs every successful reply in full.
// code is the reply's errorCode ("" for none); err is a send or wait failure.
func fwRequest(conn *session.GameConn, label, cmd string, params *sfs.SFSObject) (*sfs.SFSObject, string, error) {
	if err := conn.SendExtension(cmd, params); err != nil {
		wrapped := session.SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", cmd, "error", wrapped)
		return nil, "", wrapped
	}
	msg, err := session.WaitForCmd(conn, session.DefaultCmdTimeout, cmd)
	if err != nil {
		slog.Error(label+" no response", "cmd", cmd, "error", err)
		return nil, "", err
	}
	code := ""
	if v, ok := msg.Params.Get("errorCode"); ok {
		code = fmt.Sprintf("%v", v.Val)
	}
	slog.Debug(label, "cmd", cmd, "errorCode", code, "replyKeys", msg.Params.Keys())
	return msg.Params, code, nil
}

// fwMember is one al.rank entry. Idx, its position in the reply's list, is the only owner
// reference the logs carry: uids and names are other players' PII.
type fwMember struct {
	PointID int64
	Rank    int64
	Idx     int
}

// fwRoster is the al.rank reply: every member by uid, and the HQ points to scan.
type fwRoster struct {
	byUID                             map[string]fwMember
	points                            []int64
	listed, noPoint, away, outsideMap int
}

// fwMemberServer is where a member is now: curServerId, else serverId.
func fwMemberServer(e *sfs.SFSObject) (int64, bool) {
	for _, k := range []string{"curServerId", "serverId"} {
		if s, ok := evNum(e, k); ok && s > 0 {
			return s, true
		}
	}
	return 0, false
}

// fwOwnServer is init user.serverId (GetSelfServerId, PlayerInfo.lua:490-491, 766-781), else the
// server of the player's own al.rank entry.
func fwOwnServer(in *Init, list []*sfs.SFSObject, ownUID string) (int64, bool) {
	if s, ok := evNum(in.Object("user"), "serverId"); ok && s > 0 {
		return s, true
	}
	for _, e := range list {
		if ownUID != "" && fwMemberUID(e) == ownUID {
			return fwMemberServer(e)
		}
	}
	return 0, false
}

// fwMemberUID is AllianceMemberInfo:ParseData's uid: userId overrides uid (AllianceMemberInfo.lua:83-115).
func fwMemberUID(e *sfs.SFSObject) string {
	if u := evString(e, "userId"); u != "" {
		return u
	}
	return evString(e, "uid")
}

// fwParseRoster reads the members and keeps the HQ points on server: a member with no pointId,
// on another server (their HQ is on that server's map) or outside the 1000-tile map is listed but
// not scanned.
func fwParseRoster(list []*sfs.SFSObject, server int64) fwRoster {
	r := fwRoster{byUID: map[string]fwMember{}}
	for i, e := range list {
		uid := fwMemberUID(e)
		if uid == "" {
			continue
		}
		r.listed++
		pid, _ := evNum(e, "pointId")
		rank, _ := evNum(e, "rank")
		r.byUID[uid] = fwMember{PointID: pid, Rank: rank, Idx: i}
		s, known := fwMemberServer(e)
		switch {
		case pid <= 0:
			r.noPoint++
		case known && s != server:
			r.away++
		case pid > fwMapTiles*fwMapTiles:
			r.outsideMap++
		default:
			r.points = append(r.points, pid)
		}
	}
	return r
}

// fwBox is one chest found on a tile, with the building it sits on.
type fwBox struct {
	fireworkGift
	OwnerUID   string
	AllianceID string
}

// fwBase is a player HQ found on a tile with the statuses on it (BuildInfo field 32), for
// base-bubbles.
type fwBase struct {
	PointID               int32
	ServerID, SrcServerID int32
	OwnerUID              string
	Statuses              []worldStatus
}

// fwScan is one read of the members' HQ tiles. name prefixes its log lines; server is the map
// read. pointTypes counts the decoded points by reply key ("points", "alInfos") and pointType;
// treasures are the treasure points (pointType 21) found, each once; bases are the HQs carrying
// statuses, each once.
type fwScan struct {
	name               string
	allianceID, ownUID string
	server             int64
	roster             fwRoster
	blocks, requests   int
	failed             int
	points, alInfos    int
	decodeErrors       int
	buildings          int
	memberHQs          map[string]bool
	giftLists, shows   int
	boxes              []fwBox
	pointTypes         map[string]map[int32]int
	treasures          []worldPoint
	bases              []fwBase
	now                time.Time
	serverClock        bool
}

// fwAllianceID is init alliance.uid, else user.allianceId; "" (or "0") is no alliance.
func fwAllianceID(in *Init) string {
	for _, id := range []string{evString(in.Object("alliance"), "uid"), evString(in.Object("user"), "allianceId")} {
		if id != "" && id != "0" {
			return id
		}
	}
	return ""
}

// fireworksScan reads the members' HQ tiles. It returns nil when there is nothing to scan, and a
// partial scan with the error when some block request failed.
func fireworksScan(conn *session.GameConn, in *Init) (*fwScan, error) {
	return sharedAllianceTileScan(conn, in, "fireworks")
}

// allianceTileScanName prefixes the shared scan's log lines.
const allianceTileScanName = "alliance-tile-scan"

// fwScanRun is one run's alliance tile scan, keyed by (conn, in) like the activity sweep.
type fwScanRun struct {
	conn *session.GameConn
	in   *Init
	sc   *fwScan
}

var (
	fwScanMu    sync.Mutex
	fwScanCache *fwScanRun
)

// sharedAllianceTileScan is this run's alliance tile scan (allianceTileScan at atBlockRadius),
// run by the first feature that needs it; who names that feature in the log. The first caller
// gets the scan's error; later callers get the same scan and no error, so a failed block request
// is reported once per run, not once per feature. A new login or init scans again.
func sharedAllianceTileScan(conn *session.GameConn, in *Init, who string) (*fwScan, error) {
	fwScanMu.Lock()
	if r := fwScanCache; r != nil && r.conn == conn && r.in == in {
		fwScanMu.Unlock()
		if r.sc != nil {
			slog.Info(who+": reusing this run's alliance tile scan", "requests", r.sc.requests, "blocks", r.sc.blocks,
				"failedRequests", r.sc.failed, "chests", len(r.sc.boxes), "treasurePoints", len(r.sc.treasures))
		} else {
			slog.Info(who + ": this run's alliance tile scan found nothing to read")
		}
		return r.sc, nil
	}
	fwScanMu.Unlock()
	slog.Info(who+": reading the alliance members' HQ tiles", "blockRadius", atBlockRadius)
	sc, err := allianceTileScan(conn, in, allianceTileScanName, atBlockRadius)
	fwScanMu.Lock()
	fwScanCache = &fwScanRun{conn: conn, in: in, sc: sc}
	fwScanMu.Unlock()
	return sc, err
}

// allianceTileScan reads the world blocks holding the members' HQs and, with radius > 0, every
// block within radius blocks of each, logging as name. It returns nil when there is nothing to
// scan, and a partial scan with the error when some block request failed.
func allianceTileScan(conn *session.GameConn, in *Init, name string, radius int) (*fwScan, error) {
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil, nil
	}
	sc := &fwScan{name: name, allianceID: fwAllianceID(in), ownUID: ownUID(in), memberHQs: map[string]bool{},
		pointTypes: map[string]map[int32]int{}, now: evNow()}
	if sc.allianceID == "" {
		slog.Info(name + ": not in an alliance; skipping")
		return nil, nil
	}
	p := sfs.NewSFSObject()
	p.PutUtfString("allianceId", sc.allianceID)
	resp, code, err := fwRequest(conn, name+": alliance member list", fireworksRankCmd, p)
	if err != nil {
		return nil, err
	}
	if code != "" {
		return nil, fmt.Errorf("%s: %s: errorCode=%s", name, fireworksRankCmd, code)
	}
	list := evObjects(resp, "list")
	server, ok := fwOwnServer(in, list, sc.ownUID)
	if !ok {
		slog.Warn(name + ": own server unknown (no init user.serverId); skipping")
		return nil, nil
	}
	sc.server = server
	sc.roster = fwParseRoster(list, server)
	slog.Info(name+": alliance roster", "members", sc.roster.listed, "hqsToScan", len(sc.roster.points),
		"noPoint", sc.roster.noPoint, "onOtherServer", sc.roster.away, "outsideMap", sc.roster.outsideMap, "server", server)
	reqs := fwPlanBlocksAround(sc.roster.points, radius)
	if len(reqs) == 0 {
		slog.Info(name + ": no member HQ on this server's map to read")
		return nil, nil
	}
	var errs []error
	for i, r := range reqs {
		if i > 0 {
			fwBlockSleep(fwBlockPace)
		}
		sc.blocks += len(r.Index)
		sc.requests++
		label := fmt.Sprintf("%s: world blocks %d/%d", name, i+1, len(reqs))
		resp, code, err := fwRequest(conn, label, fireworksBlockCmd, fwBlockParams(r, server, i == 0, evNow()))
		if err != nil {
			sc.failed++
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				break
			}
			continue
		}
		if code != "" {
			sc.failed++
			slog.Warn(label+" failed", "cmd", fireworksBlockCmd, "errorCode", code)
			errs = append(errs, fmt.Errorf("%s: %s: errorCode=%s", name, fireworksBlockCmd, code))
			continue
		}
		sc.read(resp)
	}
	slog.Info(name+": member HQ tiles read", "requests", sc.requests, "failedRequests", sc.failed, "blocks", sc.blocks,
		"blockRadius", radius, "points", sc.points, "alInfos", sc.alInfos, "decodeErrors", sc.decodeErrors, "buildings", sc.buildings,
		"memberHQs", len(sc.memberHQs), "hqsScanned", len(sc.roster.points), "field54Present", sc.giftLists > 0,
		"giftLists", sc.giftLists, "fireworkShows", sc.shows, "chests", len(sc.boxes), "treasurePoints", len(sc.treasures),
		"hqsWithStatuses", len(sc.bases), "serverClock", sc.serverClock)
	return sc, errors.Join(errs...)
}

// read takes one world.get.block reply: one object, or one per server under serverPointArr
// (HandleViewPointsReply, A-CS:179920-179993). Its timeStamp (server ms) becomes the scan's clock.
func (sc *fwScan) read(resp *sfs.SFSObject) {
	if ts, ok := evNum(resp, "timeStamp"); ok && ts > 1e12 {
		sc.now, sc.serverClock = time.UnixMilli(ts), true
	}
	parts := []*sfs.SFSObject{resp}
	if resp.Has("serverPointArr") {
		parts = evObjects(resp, "serverPointArr")
	}
	for _, part := range parts {
		sc.points += sc.decodeBlobs(part, "points")
		sc.alInfos += sc.decodeBlobs(part, "alInfos")
	}
}

// decodeBlobs decodes the WorldPointInfo byte arrays under key and returns how many there were.
func (sc *fwScan) decodeBlobs(o *sfs.SFSObject, key string) int {
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	arr, _ := v.Val.(*sfs.SFSArray)
	if arr == nil {
		return 0
	}
	n := 0
	for _, it := range arr.Items() {
		b, ok := it.Val.([]byte)
		if !ok {
			continue
		}
		n++
		pt, err := decodeWorldPoint(b)
		if err != nil {
			sc.decodeErrors++
			slog.Debug(sc.name+": undecodable world point", "key", key, "bytes", len(b), "error", err)
			continue
		}
		if sc.pointTypes[key] == nil {
			sc.pointTypes[key] = map[int32]int{}
		}
		sc.pointTypes[key][pt.PointType]++
		sc.add(pt)
	}
	return n
}

// add records a decoded point's treasure, building and chests; a treasure or chest seen twice
// (points and alInfos) is kept once.
func (sc *fwScan) add(pt worldPoint) {
	if t := pt.Treasure; t != nil && !slices.ContainsFunc(sc.treasures, func(o worldPoint) bool {
		return o.ID == pt.ID && o.Treasure.UUID == t.UUID
	}) {
		sc.treasures = append(sc.treasures, pt)
	}
	bi := pt.Build
	if bi == nil {
		return
	}
	sc.buildings++
	if _, member := sc.roster.byUID[bi.OwnerUID]; member && bi.OwnerUID != "" {
		sc.memberHQs[bi.OwnerUID] = true
	}
	if bi.BuildID == claimHQBuildingID && len(bi.Statuses) > 0 && !slices.ContainsFunc(sc.bases, func(b fwBase) bool { return b.PointID == pt.ID }) {
		sc.bases = append(sc.bases, fwBase{PointID: pt.ID, ServerID: pt.ServerID, SrcServerID: pt.SrcServerID,
			OwnerUID: bi.OwnerUID, Statuses: bi.Statuses})
	}
	sc.shows += bi.Shows
	if len(bi.Gifts) > 0 {
		sc.giftLists++
	}
	for _, g := range bi.Gifts {
		if !slices.ContainsFunc(sc.boxes, func(b fwBox) bool { return b.UUID == g.UUID }) {
			sc.boxes = append(sc.boxes, fwBox{fireworkGift: g, OwnerUID: bi.OwnerUID, AllianceID: bi.AllianceID})
		}
	}
}

// eligible applies the claim rules to b at the scan's clock; claimed holds the chests this run
// already answered for.
func (sc *fwScan) eligible(b fwBox, claimed map[int64]bool) (bool, string) {
	sent := evUnix(b.SendTime)
	_, member := sc.roster.byUID[b.OwnerUID]
	switch {
	case b.UUID == 0:
		return false, "no uuid"
	case claimed[b.UUID]:
		return false, "already answered this run"
	case b.SendTime <= 0 || !sc.now.After(sent):
		return false, "not sent yet"
	case !sc.now.Before(sent.Add(fwBoxLifetime)):
		return false, "expired"
	case b.Num >= b.Max:
		return false, "full"
	case b.OwnerUID == "":
		return false, "no owner" // the claim sends ownerUid (GetFireworksGiftMessage.lua:4-8)
	case fwType(b.ConfigID) == 1 && b.AllianceID != sc.allianceID:
		return false, "type 1 from another alliance"
	case fwType(b.ConfigID) != 1 && !member:
		return false, "owner not an alliance member"
	}
	return true, ""
}

// ref names a player in the logs without their uid: "self", "member#<al.rank index>" or
// "non-member".
func (sc *fwScan) ref(uid string) string {
	switch m, member := sc.roster.byUID[uid]; {
	case uid == "":
		return "none"
	case uid == sc.ownUID:
		return "self"
	case member:
		return "member#" + strconv.Itoa(m.Idx)
	}
	return "non-member"
}

// review logs every chest with its verdict and returns the eligible ones, oldest first.
func (sc *fwScan) review(claimed map[int64]bool) []fwBox {
	var picks []fwBox
	for _, b := range sc.boxes {
		ok, why := sc.eligible(b, claimed)
		slog.Info("fireworks: chest", "uuid", b.UUID, "owner", sc.ref(b.OwnerUID), "ownerRank", sc.roster.byUID[b.OwnerUID].Rank,
			"sender", sc.ref(b.SendUID), "configId", b.ConfigID, "type", fwType(b.ConfigID), "num", b.Num, "max", b.Max,
			"ageMin", int64(sc.now.Sub(evUnix(b.SendTime)).Minutes()), "eligible", ok, "reason", why)
		if ok {
			picks = append(picks, b)
		}
	}
	slices.SortStableFunc(picks, func(a, b fwBox) int {
		return cmp.Or(cmp.Compare(a.SendTime, b.SendTime), cmp.Compare(a.UUID, b.UUID))
	})
	slog.Info("fireworks: chests", "found", len(sc.boxes), "eligible", len(picks))
	return picks
}

func runFireworksScan(conn *session.GameConn, in *Init) error {
	sc, err := fireworksScan(conn, in)
	if sc != nil {
		sc.review(nil)
	}
	return err
}

func runFireworks(conn *session.GameConn, in *Init) error {
	sc, err := fireworksScan(conn, in)
	if sc == nil || session.ContainsNonTimeoutNetError(err) {
		return err
	}
	errs := []error{err}
	claimed := map[int64]bool{}
	picks := sc.review(claimed)
	if len(picks) > fwMaxClaimsPerRun {
		slog.Info("fireworks: more eligible chests than the per-run cap; the rest wait for the next run", "eligible", len(picks), "cap", fwMaxClaimsPerRun)
		picks = picks[:fwMaxClaimsPerRun]
	}
	got := 0
claims:
	for i, b := range picks {
		if i > 0 {
			fwSleep(fireworksClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", b.UUID)
		p.PutUtfString("ownerUid", b.OwnerUID)
		p.PutInt("type", fwType(b.ConfigID))
		resp, code, err := fwRequest(conn, "fireworks: chest claim", fireworksGiftCmd, p)
		if err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				break
			}
			continue
		}
		claimed[b.UUID] = true
		who := []any{"uuid", b.UUID, "owner", sc.ref(b.OwnerUID), "configId", b.ConfigID}
		switch {
		case code == "":
			got++
			slog.Info("fireworks: claimed a chest", append(who, "isDouble", evBool(resp, "isDouble"), "reward", fwReward(resp),
				"replyKeys", resp.Keys())...)
		case fireworksBenignCodes[code] != "":
			slog.Info("fireworks: chest not claimable (expected)", append(who, "errorCode", code, "meaning", fireworksBenignCodes[code])...)
		case code == fwDailyCapCode:
			slog.Warn("fireworks: daily chest cap reached; stopping this run", append(who, "errorCode", code)...)
			break claims
		case code == fwSlowDownCode:
			slog.Warn("fireworks: the server asked to slow down; stopping this run", append(who, "errorCode", code)...)
			errs = append(errs, fmt.Errorf("fireworks: %s: errorCode=%s (slow down)", fireworksGiftCmd, code))
			break claims
		default:
			slog.Error("fireworks: chest claim failed", append(who, "errorCode", code)...)
			errs = append(errs, fmt.Errorf("fireworks: %s uuid %d: errorCode=%s", fireworksGiftCmd, b.UUID, code))
		}
	}
	slog.Info("fireworks: claims done", "claimed", got, "answered", len(claimed), "eligible", len(picks))
	return errors.Join(errs...)
}

// fwReward formats the claim reply's reward for the log; it holds items, not player data.
func fwReward(resp *sfs.SFSObject) string {
	v, ok := resp.Get("reward")
	if !ok {
		return ""
	}
	o := sfs.NewSFSObject()
	o.PutValue("reward", v)
	return o.StringRedacted()
}
