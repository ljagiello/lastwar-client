package game

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Dig games (MASTER.md §4 rows 68, 89, 121): the event dig games the client runs without the chat
// channel, each an opt-in feature with a read-only -plan twin.
//
//   - treasure-hunt (feature_treasure_hunt.go): the Treasure Hunt card event, v1 (activity type
//     130, DigActivityManager) and v2 (type 139, ActivityTreasureHuntNewManager).
//   - season-dig, offseason-dig and alliance-boss-dig (feature_dig_vaults.go): the vault games on
//     the shared DiggingMapData model. These are the S3 Archaeologist vaults (type 241,
//     DiggingDataManager), the off-season Street Rush alliance vault (type 260,
//     OffSeasonDiggingDataManager) and the plague-boss vault (AllyDrill type 202,
//     AllyDrillDataManager).
//
// Every request goes through digsCheckRequest. The command must be one of digsAllowedKeys and
// carry exactly its keys, so the purchase commands can never go out: buy.activity.dig.goods and
// buy.activity.dig.v2.goods {activityId, num} cost 100 diamonds a pickaxe (activity_dig.diamond_buy
// "100;200"). Nor can any gold or useGold key. No dig action scores in the Alliance Duel (no score
// row in duel_scores_gen.go or DUEL.md §2 counts a dig, a vault or the pickaxe item), so none of
// these features declares Duel or Hold.
const (
	digsMaxWrites   = 200      // choose, dig, open and claim requests per feature run
	digsNoItemsCode = "120021" // "Not enough items." (the client's tip when a pickaxe dig finds none)
)

// digsAllowedKeys is every command the dig features may send, with its exact key set (the Lua
// OnCreate of each message, cited at the feature that sends it).
var digsAllowedKeys = map[string][]string{
	// treasure-hunt, read
	treasureHuntInfoCmd:   {"activityId"},
	treasureHuntInfoV2Cmd: {"activityId"},
	// treasure-hunt, write
	treasureHuntChooseCmd:   {"activityId", "bigRewardIndex"},
	treasureHuntChooseV2Cmd: {"activityId", "bigRewardIndex"},
	treasureHuntDigCmd:      {"activityId", "digIndex"},
	treasureHuntDigV2Cmd:    {"activityId", "digIndex"},
	treasureHuntTierCmd:     {"aid"},
	treasureHuntStoredCmd:   {"aid"},
	// vaults, read
	seasonDigListCmd:    {},
	seasonDigInfoCmd:    {"type", "uuid", "uid"},
	offSeasonDigListCmd: {},
	offSeasonDigInfoCmd: {"uuid"},
	allianceBossActCmd:  {"serverId"},
	allianceBossInfoCmd: {},
	// vaults, write
	seasonDigOpenCmd:      {"type", "uuid", "uid", "pos"},
	seasonDigPersonalCmd:  {"uuid"},
	seasonDigBlockCmd:     {"uuid", "pos"},
	offSeasonDigOpenCmd:   {"uuid", "pos"},
	offSeasonDigBlockCmd:  {"uuid", "pos"},
	allianceBossOpenCmd:   {"uuid", "pos"},
	allianceBossRewardCmd: {"uuid", "pos"},
}

// digsReadOnly is the subset of digsAllowedKeys a -plan run may send: the info and list requests,
// which change nothing on the server.
var digsReadOnly = map[string]bool{
	treasureHuntInfoCmd:   true,
	treasureHuntInfoV2Cmd: true,
	seasonDigListCmd:      true,
	seasonDigInfoCmd:      true,
	offSeasonDigListCmd:   true,
	offSeasonDigInfoCmd:   true,
	allianceBossActCmd:    true,
	allianceBossInfoCmd:   true,
}

// digsCheckRequest is the last check before a dig request goes on the wire: a known command with
// exactly its keys, never a buy command and never a gold key.
func digsCheckRequest(cmd string, p *sfs.SFSObject) error {
	if strings.Contains(strings.ToLower(cmd), "buy") {
		return fmt.Errorf("refusing purchase command %s", cmd)
	}
	keys, ok := digsAllowedKeys[cmd]
	if !ok {
		return fmt.Errorf("%s is not an allowed dig command", cmd)
	}
	for _, k := range p.Keys() {
		if strings.Contains(strings.ToLower(k), "gold") {
			return fmt.Errorf("%s: refusing key %q", cmd, k)
		}
		if !slices.Contains(keys, k) {
			return fmt.Errorf("%s: refusing unexpected key %q", cmd, k)
		}
	}
	for _, k := range keys {
		if !p.Has(k) {
			return fmt.Errorf("%s: missing %q", cmd, k)
		}
	}
	return nil
}

// digsTrip disables the dig features' writes for the rest of the process once a reply shows the
// diamond balance falling.
var digsTrip struct {
	sync.Mutex
	reason string
}

func digsTripped() string {
	digsTrip.Lock()
	defer digsTrip.Unlock()
	return digsTrip.reason
}

func digsSetTrip(reason string) {
	digsTrip.Lock()
	defer digsTrip.Unlock()
	if digsTrip.reason == "" {
		digsTrip.reason = reason
	}
}

// digsResult is how a write went.
type digsResult int

const (
	digsDone    digsResult = iota // sent and accepted, or (plan) would be sent
	digsRefused                   // the server answered with an errorCode
	digsFailed                    // not sent or no reply: refused locally, halted, dead connection
)

// digsRun is one feature run's sender. Reads (digsReadOnly) go out in a plan too; writes are
// logged instead. Every reply is checked against the diamond balance: init user.gold
// (PlayerInfo.lua:612-613), or the first balance a reply carries, must never fall.
type digsRun struct {
	conn    *session.GameConn
	feature string
	dry     bool
	gold    int64
	goldSet bool
	writes  int
	errs    []error
	halt    bool
}

func newDigsRun(conn *session.GameConn, in *Init, feature string, dry bool) *digsRun {
	r := &digsRun{conn: conn, feature: feature, dry: dry}
	if g, ok := claimInt(in.Object("user"), "gold"); ok {
		r.gold, r.goldSet = g, true
	}
	return r
}

func (r *digsRun) add(err error) {
	if err == nil {
		return
	}
	r.errs = append(r.errs, err)
	if session.ContainsNonTimeoutNetError(err) {
		r.halt = true
	}
}

func (r *digsRun) err() error { return errors.Join(r.errs...) }

// read sends an info request and returns its reply, or nil when it failed or carried an
// errorCode.
func (r *digsRun) read(label, cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	if r.halt {
		return nil
	}
	if !digsReadOnly[cmd] {
		r.add(fmt.Errorf("%s: %s is not a read-only dig command", label, cmd))
		r.halt = true
		return nil
	}
	if err := digsCheckRequest(cmd, p); err != nil {
		r.add(fmt.Errorf("%s: %w", label, err))
		r.halt = true
		return nil
	}
	msg, err := session.SendAndWait(r.conn, label, cmd, p)
	if err != nil || msg == nil {
		r.add(err)
		return nil
	}
	if msg.Params.Has("errorCode") {
		return nil
	}
	r.checkGold(label, cmd, msg.Params)
	return msg.Params
}

// write sends a choose, dig, open or claim request, or in a plan logs it. It returns the reply
// (nil in a plan) and the outcome; an errorCode reply comes back with digsRefused.
func (r *digsRun) write(label, cmd string, p *sfs.SFSObject) (*sfs.SFSObject, digsResult) {
	if r.halt {
		return nil, digsFailed
	}
	if err := digsCheckRequest(cmd, p); err != nil {
		r.add(fmt.Errorf("%s: %w", label, err))
		r.halt = true
		return nil, digsFailed
	}
	if digsReadOnly[cmd] {
		r.add(fmt.Errorf("%s: %s is a read, not a write", label, cmd))
		r.halt = true
		return nil, digsFailed
	}
	if r.writes >= digsMaxWrites {
		slog.Warn(r.feature+": action cap reached; stopping this run", "cap", digsMaxWrites)
		r.halt = true
		return nil, digsFailed
	}
	r.writes++
	if r.dry {
		slog.Info(r.feature+" plan: would send", "what", label, "cmd", cmd, "params", p.StringRedacted())
		return nil, digsDone
	}
	if reason := digsTripped(); reason != "" {
		r.add(fmt.Errorf("%s: dig writes are disabled for this session by the diamond tripwire: %s", r.feature, reason))
		r.halt = true
		return nil, digsFailed
	}
	msg, err := session.SendAndWait(r.conn, label, cmd, p)
	if msg == nil {
		r.add(err)
		r.halt = true // no reply: whether it went through is unknown
		return nil, digsFailed
	}
	if msg.Params.Has("errorCode") {
		r.add(err) // nil for a registered benign code
		return msg.Params, digsRefused
	}
	r.checkGold(label, cmd, msg.Params)
	return msg.Params, digsDone
}

// checkGold is the diamond tripwire: every gold or remainGold a reply carries, at the top level or
// under resource, must be at least the last balance seen.
func (r *digsRun) checkGold(label, cmd string, reply *sfs.SFSObject) {
	for _, h := range []*sfs.SFSObject{reply, evObject(reply, "resource")} {
		if h == nil {
			continue
		}
		for _, key := range []string{"gold", "remainGold"} {
			g, ok := claimInt(h, key)
			if !ok {
				continue
			}
			if r.goldSet && g < r.gold {
				reason := fmt.Sprintf("%s (%s): diamonds fell from %d to %d", label, cmd, r.gold, g)
				digsSetTrip(reason)
				slog.Error(r.feature+" DIAMOND TRIPWIRE: dig writes disabled for this session", "call", cmd, "reason", reason)
				r.add(errors.New(r.feature + " diamond tripwire: " + reason))
				r.halt = true
				return
			}
			r.gold, r.goldSet = g, true
		}
	}
}
