package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel ticket recruits (DUEL.md §4.2 "Pulls", §5, §7.3). OPT-IN: it spends recruit
// tickets. The scheduler releases the feature on a day that scores either kind of recruit, so Run
// checks each kind again: hero tickets are spent only when today scores type 42 (Train Heroes,
// Thu) and survivor tickets only when it scores type 120 (Base Expansion, Tue). Every ticket held
// is spent; unlike the client's hero_recruit_100_tips3 prompt it does not stop at the last chest
// (UIHeroRecruitView.lua:1228-1235). At most duelRecruitMaxPulls requests are sent per run, and
// none after the duel day ends.
//
// Hero pools come from init lotteryFreeInfo. Pools of a type other than 1 are dropped; the client
// drops types 2 and 3 (LotteryDataManager.lua:121). Closed pools are dropped (IsOpen; start and
// end are in seconds, LotteryInfo.lua:176-181, 323-329), as are Expert pools outside their window
// (GetOtherLotteryDict, LotteryDataManager.lua:188-201) and pools with an extra_res cost
// (LotteryInfo.lua:244-252). The rest are taken in the client's tab order, order descending
// (UIHeroRecruitCtrl.lua:21-42), with ties broken by id. For each ticket item held, the first pool
// whose single cost (item "itemId;num|itemId;num", LotteryInfo.lua:152-162) is that ticket is the
// one pulled. That is the pool the recruit view sends for its current tab.
//
// A single pull is `lottery.hero.card {id, isTen: 0, useFree: 0, aiPushStatus, itemId}`. The
// client sends it only when the held count >= the single cost (UIHeroRecruitView.lua:963-975). A
// ten pull is isTen 1 with the second cost entry (:997-1048; GetItemCost :1213-1219). Here it is
// sent only when that entry is the same ticket and the count >= max(10, its cost). isTen 2 (the
// hundred pull) and lotteryCount are never sent.
//
// A survivor pull is `lottery.worker.card {useFree: 0, isTen: 0|1, officerId}`
// (LotteryWorkerCardMessage.lua:4-9; CenterWorkerNewContentContainer.lua:424-435, 438-465), with
// officerId the first init workerOfficerIds entry. The ticket is 230015, 1 for a single pull and 10
// for a ten pull (WorkerLotteryInfo.lua GetCostItems). That id is officer_recruit#401100 item_extra,
// and DataConfig worker_recruit_1.k3 as the fallback, both 230015 in 1.0.364.
//
// Free pulls: while a pool's free pull is due, the client's single button sends useFree 1, and only
// its ten button sends useFree 0 (UIHeroRecruitView.lua:931-944, 997-1048; the survivor panel does
// the same, CenterWorkerNewContentContainer.lua:399-465). So while a free pull is due, or its timer
// is unknown, only ten pulls are sent here. The free pull itself belongs to free-recruits.
//
// Neither pull reply carries the ticket count: hero replies carry lotteryCount, totalLotteryNum
// and id, and survivor replies carry reward and resource. The client learns the count from
// push.item.* pushes (ItemData.lua:215-307). So the count here is a ledger. It starts at the
// largest init items stack of the ticket (GetItemById reads one stack, ItemData.lua:38-47). The
// cost is deducted before each pull is sent. Any count a push.item.use, push.item.del,
// push.item.add or push.batch.item.add carries while a reply is awaited lowers it, and nothing
// raises it. A pull is sent only when the ledger covers its cost. Static-only: not sent live yet.
const (
	duelRecruitHeroCmd       = freeRecruitsHeroCmd
	duelRecruitWorkerCmd     = freeRecruitsWorkerCmd
	duelRecruitWorkerInfoCmd = freeRecruitsWorkerInfoCmd
	duelRecruitWorkerTicket  = "230015"
	duelRecruitTen           = 10
	// duelRecruitMaxPulls bounds the pull requests per run, hero and survivor together; a ten
	// pull is one request.
	duelRecruitMaxPulls = 100
)

func init() {
	registerFeature(Feature{
		Name:    "duel-recruit-tickets",
		Summary: "OPT-IN: on Alliance Duel days that score recruits, spend every hero (Thu) / survivor (Tue) recruit ticket held; tickets only, diamond tripwire",
		Duel:    []DuelScore{{Type: DuelScoreRecruitHero}, {Type: DuelScoreRecruitSurvivor}},
		Hold:    true,
		Run:     runDuelRecruitTickets,
	})
}

// duelRecruitCost is one "itemId;num" entry of a pool's cost.
type duelRecruitCost struct {
	item string
	num  int64
}

// duelRecruitPool is one hero pool that can be pulled with tickets.
type duelRecruitPool struct {
	id      string
	order   int64
	single  duelRecruitCost
	ten     duelRecruitCost // num 0 when the pool has no ten pull
	freeDue bool            // the free pull is due now, or its timer is unknown
}

// duelRecruitCosts parses a pool's item string, "itemId;num|itemId;num". ok is false unless the
// first entry has an item and a positive count.
func duelRecruitCosts(item string) (single, ten duelRecruitCost, ok bool) {
	var costs []duelRecruitCost
	for part := range strings.SplitSeq(item, "|") {
		id, num, _ := strings.Cut(strings.TrimSpace(part), ";")
		n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if err != nil {
			n = 0
		}
		costs = append(costs, duelRecruitCost{item: strings.TrimSpace(id), num: n})
	}
	if len(costs) == 0 || costs[0].item == "" || costs[0].num <= 0 {
		return single, ten, false
	}
	single = costs[0]
	if len(costs) > 1 && costs[1].item != "" && costs[1].num > 0 {
		ten = costs[1]
	}
	return single, ten, true
}

// duelRecruitHeroPools returns the ticket-pullable hero pools in the client's tab order.
func duelRecruitHeroPools(in *Init) []duelRecruitPool {
	now := evNow().Unix()
	var out []duelRecruitPool
	for _, p := range in.Objects("lotteryFreeInfo") {
		id := evString(p, "id")
		if id == "" {
			continue
		}
		if t, ok := evNum(p, "type"); ok && t != 1 {
			continue
		}
		if evString(p, "extra_res") != "" {
			continue
		}
		start, _ := evNum(p, "startTime")
		end, _ := evNum(p, "endTime")
		if end > start && (now <= start || now >= end) {
			continue
		}
		if evString(p, "timeType") == "3" && end <= start {
			continue
		}
		single, ten, ok := duelRecruitCosts(evString(p, "item"))
		if !ok {
			continue
		}
		limit, _ := evNum(p, "dailyFreeLimit")
		next, hasNext := evNum(p, "dailyFreeNextFreshTime")
		pool := duelRecruitPool{id: id, single: single, ten: ten,
			freeDue: limit > 0 && (!hasNext || next <= 0 || now >= next)}
		pool.order, _ = evNum(p, "order")
		out = append(out, pool)
	}
	slices.SortStableFunc(out, func(a, b duelRecruitPool) int {
		if c := cmp.Compare(b.order, a.order); c != 0 {
			return c
		}
		ai, aerr := strconv.ParseInt(a.id, 10, 64)
		bi, berr := strconv.ParseInt(b.id, 10, 64)
		if aerr == nil && berr == nil {
			return cmp.Compare(ai, bi)
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

// duelInitItemCounts returns the largest init items stack per item id. The client reads items as
// {uuid, itemId or goodsId, count} (ItemInfo.lua:35-64), as an array or keyed by uuid.
func duelInitItemCounts(in *Init) map[string]int64 {
	out := map[string]int64{}
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		id := evString(it, "itemId")
		if id == "" {
			id = evString(it, "goodsId")
		}
		n, ok := evNum(it, "count")
		if !ok {
			n, ok = evNum(it, "num")
		}
		if id == "" || !ok || n <= 0 {
			continue
		}
		out[id] = max(out[id], n)
	}
	return out
}

// duelPoints is the base points one unit of (typ, value) earns today, or 0 when it scores nothing.
func duelPoints(d *DuelDay, typ int32, value string) int64 {
	if d == nil {
		return 0
	}
	for _, id := range d.ScoreIDs {
		if r, ok := duelScoreTable[id]; ok && r.typ == typ && (value == "" || r.value == value) {
			return r.points
		}
	}
	return 0
}

// duelRecruitRun is one run's state: the guard, today's duel and the pulls sent so far.
type duelRecruitRun struct {
	g       *duelSpendGuard
	d       *DuelDay
	pulls   int
	stopped bool // the gold check failed: nothing more is spent this run
}

// canPull reports whether another pull may be sent, logging why not.
func (r *duelRecruitRun) canPull() bool {
	if r.pulls >= duelRecruitMaxPulls {
		slog.Info("duel-recruit-tickets: pull cap reached for this run", "pulls", r.pulls)
		return false
	}
	if !evNow().Before(r.d.End) {
		slog.Info("duel-recruit-tickets: the duel day ended; stopping", "dayEnd", r.d.End)
		return false
	}
	return true
}

func runDuelRecruitTickets(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("duel-recruit-tickets: no init push; skipping")
		return nil
	}
	if err := duelSpendHaltedErr("duel-recruit-tickets"); err != nil {
		return err
	}
	d := TodayDuel(conn, in)
	hero, survivor := d.Scores(DuelScoreRecruitHero, ""), d.Scores(DuelScoreRecruitSurvivor, "")
	if !hero && !survivor {
		slog.Info("duel-recruit-tickets: today's duel scores no recruits; tickets kept", "theme", d.ThemeName())
		return nil
	}
	held := duelInitItemCounts(in)
	ledger := map[string]int64{}
	var pools []duelRecruitPool
	if hero {
		for _, p := range duelRecruitHeroPools(in) {
			if held[p.single.item] >= p.single.num {
				pools = append(pools, p)
				ledger[p.single.item] = held[p.single.item]
			}
		}
	}
	if survivor && held[duelRecruitWorkerTicket] > 0 {
		ledger[duelRecruitWorkerTicket] = held[duelRecruitWorkerTicket]
	}
	if len(ledger) == 0 {
		slog.Info("duel-recruit-tickets: no recruit ticket held for today's recruit type", "theme", d.ThemeName(),
			"hero", hero, "survivor", survivor)
		return nil
	}
	g, err := newDuelSpendGuard(conn, in, "duel-recruit-tickets")
	if g == nil {
		return err
	}
	g.items = ledger
	r := &duelRecruitRun{g: g, d: d}
	var errs []error
	if hero {
		errs = append(errs, r.heroes(pools)...)
	}
	if survivor && !r.stopped && !duelSpendStop(errs) {
		if err := r.survivors(in); err != nil {
			errs = append(errs, err)
		}
	}
	slog.Info("duel-recruit-tickets: done", "pullRequests", r.pulls, "ticketsLeft", fmt.Sprint(g.items))
	return errors.Join(errs...)
}

// heroes spends the hero tickets, one pool per ticket.
func (r *duelRecruitRun) heroes(pools []duelRecruitPool) []error {
	var errs []error
	points := duelPoints(r.d, DuelScoreRecruitHero, "")
	done := map[string]bool{}
	for _, pool := range pools {
		ticket := pool.single.item
		if done[ticket] {
			continue
		}
		done[ticket] = true
		recruited := int64(0)
		for r.canPull() {
			have := r.g.items[ticket]
			isTen, cost, units := int32(0), pool.single.num, int64(1)
			switch {
			case pool.ten.item == ticket && have >= max(duelRecruitTen, pool.ten.num):
				isTen, cost, units = 1, pool.ten.num, duelRecruitTen
			case have < pool.single.num:
				cost = 0
			case pool.freeDue:
				slog.Info("duel-recruit-tickets: the pool's free pull is due, so single pulls wait for free-recruits",
					"pool", pool.id, "ticket", ticket, "held", have)
				cost = 0
			}
			if cost == 0 {
				break
			}
			p := sfs.NewSFSObject()
			p.PutUtfString("id", pool.id)
			p.PutInt("isTen", isTen)
			p.PutInt("useFree", 0)
			push := sfs.NewSFSObject()
			for _, k := range freeRecruitsAIPushSwitches {
				push.PutBool(k, true)
			}
			p.PutSFSObject("aiPushStatus", push)
			p.PutUtfString("itemId", ticket)
			r.g.items[ticket] = have - cost
			r.pulls++
			label := fmt.Sprintf("duel hero recruit pool %s x%d", pool.id, units)
			if _, err := r.g.spend(label, duelRecruitHeroCmd, p); err != nil {
				errs = append(errs, err)
				break
			}
			recruited += units
			if err := r.g.check(); err != nil {
				errs = append(errs, err)
				r.stopped = true
				break
			}
		}
		if recruited > 0 {
			slog.Info("duel-recruit-tickets: hero recruits done", "pool", pool.id, "ticket", ticket,
				"recruited", recruited, "ticketsLeft", r.g.items[ticket], "expectedBasePoints", recruited*points)
		}
		if r.stopped || duelSpendStop(errs) {
			return errs
		}
	}
	return errs
}

// survivors spends the survivor tickets on the first officer pool.
func (r *duelRecruitRun) survivors(in *Init) error {
	ticket := duelRecruitWorkerTicket
	if r.g.items[ticket] <= 0 {
		return nil
	}
	ids := claimInts(in.Raw, "workerOfficerIds")
	if len(ids) == 0 {
		slog.Info("duel-recruit-tickets: init has no workerOfficerIds; survivor tickets kept")
		return nil
	}
	officer := int32(ids[0])
	q := sfs.NewSFSObject()
	q.PutInt("officerId", officer)
	info, err := r.g.exchange("survivor recruit info", duelRecruitWorkerInfoCmd, q)
	if err == nil && info.Params.Has("errorCode") {
		err = fmt.Errorf("survivor recruit info: errorCode=%v", info.Params.GetString("errorCode"))
	}
	if err != nil {
		return err
	}
	if r.g.tripped != nil {
		return r.g.tripped
	}
	next, ok := evNum(info.Params, "nextFreeTime")
	freeDue := !ok || next <= 0 || evNow().UnixMilli() >= next
	points := duelPoints(r.d, DuelScoreRecruitSurvivor, "")
	recruited := int64(0)
	defer func() {
		if recruited > 0 {
			slog.Info("duel-recruit-tickets: survivor recruits done", "officerId", officer, "recruited", recruited,
				"ticketsLeft", r.g.items[ticket], "expectedBasePoints", recruited*points)
		}
	}()
	for r.canPull() {
		have := r.g.items[ticket]
		isTen, cost := int32(0), int64(1)
		switch {
		case have >= duelRecruitTen:
			isTen, cost = 1, duelRecruitTen
		case have < 1:
			return nil
		case freeDue:
			slog.Info("duel-recruit-tickets: the free survivor draw is due, so single draws wait for free-recruits", "held", have)
			return nil
		}
		p := sfs.NewSFSObject()
		p.PutInt("useFree", 0)
		p.PutInt("isTen", isTen)
		p.PutInt("officerId", officer)
		r.g.items[ticket] = have - cost
		r.pulls++
		if _, err := r.g.spend(fmt.Sprintf("duel survivor recruit x%d", cost), duelRecruitWorkerCmd, p); err != nil {
			return err
		}
		recruited += cost
		if err := r.g.check(); err != nil {
			return err
		}
	}
	return nil
}

// Diamond tripwire and ledgers shared by duel-recruit-tickets and duel-troop-training (DUEL.md
// §7.3). Every spend goes through duelSpendGuard.spend. Unlike session.SendAndWait, it reads every
// message that arrives while the reply is awaited, because the balances the client updates come
// in pushes, not in the spend replies.
//   - Gold (diamonds): the baseline is init user.gold (PlayerInfo.lua:612-613); without it nothing is
//     spent. hero.dispatch.list {} is read-only and returns the live balance as gold
//     (ActDispatchTaskDataManager.lua:364-366). It is read before the first spend and after each
//     one, when it carried gold the first time. gold and remainGold are also read from the spend
//     replies, and from push.resource.info (PushResourceInfo.lua:6-17),
//     push.update.player.remain.gold and push.remain.gold.when.errorcode.triggered. A higher
//     reading raises the baseline (rewards). Any lower reading halts every duel spend for the rest
//     of the process, logs an ERROR and fails the feature.
//   - Ledgers (ticket counts, food and iron): set by the feature from init and lowered, never
//     raised, by the counts pushes and replies carry.
const (
	duelGoldProbeCmd = secretTasksListCmd

	duelPushResourceInfo  = "push.resource.info"
	duelPushRemainGold    = "push.update.player.remain.gold"
	duelPushErrRemainGold = "push.remain.gold.when.errorcode.triggered"
	duelPushItemUse       = "push.item.use"
	duelPushItemDel       = "push.item.del"
	duelPushItemAdd       = "push.item.add"
	duelPushItemBatchAdd  = "push.batch.item.add"
)

// duelSpendHalted is set when the tripwire fires; it stops every duel spend for the process.
var duelSpendHalted atomic.Bool

// errDuelSpendHalted is what a duel spend feature returns once the tripwire has fired.
var errDuelSpendHalted = errors.New("duel spending is halted for this session: the diamond balance fell during a duel spend")

// duelSpendHaltedErr returns errDuelSpendHalted, logged at ERROR, once the tripwire has fired.
func duelSpendHaltedErr(feature string) error {
	if !duelSpendHalted.Load() {
		return nil
	}
	slog.Error(feature + ": not spending: the diamond tripwire fired earlier this session")
	return errDuelSpendHalted
}

// duelSpendStop reports whether errs holds a reason to stop every further spend: the tripwire, or
// a dead connection.
func duelSpendStop(errs []error) bool {
	for _, err := range errs {
		if errors.Is(err, errDuelSpendHalted) || session.ContainsNonTimeoutNetError(err) {
			return true
		}
	}
	return duelSpendHalted.Load()
}

type duelSpendGuard struct {
	conn    *session.GameConn
	feature string
	gold    int64            // highest diamond balance seen
	probe   bool             // the gold check carried gold at the start
	tripped error            // set when the balance fell
	items   map[string]int64 // ticket ledger by item id
	res     map[string]int64 // resource ledger by init resource key (metal, money)
}

// newDuelSpendGuard arms the tripwire. It returns nil and no error when init has no gold balance
// (the feature then spends nothing), and nil and an error when the gold check already shows a
// lower balance or the connection is dead.
func newDuelSpendGuard(conn *session.GameConn, in *Init, feature string) (*duelSpendGuard, error) {
	if err := duelSpendHaltedErr(feature); err != nil {
		return nil, err
	}
	gold, ok := evNum(in.Object("user"), "gold")
	if !ok || gold < 0 {
		slog.Info(feature + ": init has no user.gold, so the diamond tripwire can't be armed; not spending")
		return nil, nil
	}
	g := &duelSpendGuard{conn: conn, feature: feature, gold: gold, items: map[string]int64{}, res: map[string]int64{}}
	msg, err := g.exchange("duel gold check", duelGoldProbeCmd, sfs.NewSFSObject())
	switch {
	case g.tripped != nil:
		return nil, g.tripped
	case err != nil && session.ContainsNonTimeoutNetError(err):
		return nil, err
	case err == nil && !msg.Params.Has("errorCode") && msg.Params.Has("gold"):
		g.probe = true
	default:
		slog.Warn(feature+": the gold check returned no balance; the tripwire reads init gold, the replies and pushed balances only",
			"cmd", duelGoldProbeCmd, "error", err)
	}
	return g, nil
}

// exchange sends cmd and waits for its reply, passing every message read meanwhile to observe.
// The error is only a send or wait failure; an errorCode reply is returned as is.
func (g *duelSpendGuard) exchange(label, cmd string, params *sfs.SFSObject) (*session.ExtensionMessage, error) {
	if err := g.conn.SendExtension(cmd, params); err != nil {
		wrapped := session.SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", cmd, "error", wrapped)
		return nil, wrapped
	}
	var reply *session.ExtensionMessage
	_, err := session.WaitFor(g.conn, session.DefaultCmdTimeout, func(e *session.Envelope) bool {
		msg, ok := e.AsExtension()
		if !ok {
			return false
		}
		g.observe(msg, cmd)
		if msg.Cmd == cmd {
			reply = msg
			return true
		}
		return false
	})
	if err != nil {
		slog.Error(label+" no response", "cmd", cmd, "error", err)
		return nil, err
	}
	return reply, nil
}

// spend sends one spend request. It refuses once the tripwire has fired, and fails on an errorCode
// reply or a balance drop seen while waiting.
func (g *duelSpendGuard) spend(label, cmd string, params *sfs.SFSObject) (*session.ExtensionMessage, error) {
	if g.tripped != nil {
		return nil, g.tripped
	}
	if err := duelSpendHaltedErr(g.feature); err != nil {
		return nil, err
	}
	msg, err := g.exchange(label, cmd, params)
	if err != nil {
		return nil, err
	}
	if g.tripped != nil {
		return msg, g.tripped
	}
	if v, ok := msg.Params.Get("errorCode"); ok {
		slog.Error(label+" failed", "cmd", cmd, "errorCode", v.Val, "response", msg.Params.StringRedacted())
		return msg, fmt.Errorf("%s: errorCode=%v", label, v.Val)
	}
	slog.Info(label, "cmd", cmd, "response", msg.Params.StringRedacted())
	return msg, nil
}

// check re-reads the balance after a spend, when the gold check worked at the start, and returns
// the tripwire's error, if it fired.
func (g *duelSpendGuard) check() error {
	if g.tripped != nil || !g.probe {
		return g.tripped
	}
	msg, err := g.exchange("duel gold check", duelGoldProbeCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	if g.tripped != nil {
		return g.tripped
	}
	if msg.Params.Has("errorCode") || !msg.Params.Has("gold") {
		return fmt.Errorf("%s: the gold check stopped returning the balance; not spending further", g.feature)
	}
	return nil
}

// observe reads the balances a message carries. sent is the command whose reply is awaited.
func (g *duelSpendGuard) observe(msg *session.ExtensionMessage, sent string) {
	p := msg.Params
	switch msg.Cmd {
	case sent, duelGoldProbeCmd, duelPushResourceInfo, duelPushRemainGold, duelPushErrRemainGold:
		for _, o := range []*sfs.SFSObject{p, evObject(p, "resource"), evObject(p, "resources")} {
			for _, k := range []string{"gold", "remainGold"} {
				if n, ok := evNum(o, k); ok {
					g.seeGold(msg.Cmd, n)
				}
			}
			if o != p || msg.Cmd == duelPushResourceInfo {
				for k := range g.res {
					if n, ok := evNum(o, k); ok && n < g.res[k] {
						slog.Info(g.feature+": a pushed resource balance is lower than the ledger; using it", "resource", k,
							"ledger", g.res[k], "pushed", n, "cmd", msg.Cmd)
						g.res[k] = n
					}
				}
			}
		}
	}
	switch msg.Cmd {
	case duelPushItemUse, duelPushItemDel, duelPushItemAdd:
		g.seeItem(msg.Cmd, p)
	case duelPushItemBatchAdd:
		for _, e := range evObjects(p, "changeArray") {
			g.seeItem(msg.Cmd, e)
		}
	}
}

func (g *duelSpendGuard) seeGold(src string, n int64) {
	switch {
	case n > g.gold && g.tripped == nil:
		g.gold = n
	case n < g.gold && g.tripped == nil:
		duelSpendHalted.Store(true)
		g.tripped = fmt.Errorf("%s: %w (%d -> %d, seen in %s)", g.feature, errDuelSpendHalted, g.gold, n, src)
		slog.Error("DIAMOND TRIPWIRE: the diamond balance fell during a duel spend; halting every duel spend for this session",
			"feature", g.feature, "before", g.gold, "after", n, "cmd", src)
	}
}

// seeItem lowers the ledger of a tracked item to the count a push carries. push.item.del removes
// the stack, so without a count it reads as 0.
func (g *duelSpendGuard) seeItem(src string, o *sfs.SFSObject) {
	id := evString(o, "itemId")
	if id == "" {
		id = evString(o, "goodsId")
	}
	cur, tracked := g.items[id]
	if !tracked {
		return
	}
	n, ok := int64(0), false
	for _, k := range []string{"count", "num", "itemLeftCnt"} {
		if n, ok = evNum(o, k); ok {
			break
		}
	}
	if !ok {
		if src != duelPushItemDel {
			return
		}
		n = 0
	}
	if n < cur {
		slog.Info(g.feature+": a pushed ticket count is lower than the ledger; using it", "item", id, "ledger", cur,
			"pushed", n, "cmd", src)
		g.items[id] = n
	}
}

// duelFloat reads a numeric field as a float64 (init effect values are fractions); ok is false
// when it is absent or not a number.
func duelFloat(o *sfs.SFSObject, key string) (float64, bool) {
	v, ok := o.Get(key)
	if !ok {
		return 0, false
	}
	switch n := v.Val.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		f := float64(n)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	n, ok := evNum(o, key)
	return float64(n), ok
}
