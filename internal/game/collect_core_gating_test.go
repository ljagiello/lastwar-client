package game

import (
	"slices"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// scriptedServer answers every request arriving on a NewPipeGameConnPair server end with
// reply(msg) until the pipe closes, recording the requests in order. reply returns the response
// cmd ("" echoes the request's) and params (nil sends an empty object). Unlike a fixed-count
// fake server, it never needs to know how many requests a gate lets through, so a test can assert
// the exact sequence afterwards, including that a gated request never went out.
type scriptedServer struct {
	mu   sync.Mutex
	reqs []*session.ExtensionMessage
}

func serveScripted(server *session.GameConn, reply func(msg *session.ExtensionMessage) (string, *sfs.SFSObject)) *scriptedServer {
	s := &scriptedServer{}
	go func() {
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, msg)
			s.mu.Unlock()
			cmd, resp := "", (*sfs.SFSObject)(nil)
			if reply != nil {
				cmd, resp = reply(msg)
			}
			if cmd == "" {
				cmd = msg.Cmd
			}
			if resp == nil {
				resp = sfs.NewSFSObject()
			}
			if err := server.SendExtension(cmd, resp); err != nil {
				return
			}
		}
	}()
	return s
}

func (s *scriptedServer) cmds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reqs))
	for i, m := range s.reqs {
		out[i] = m.Cmd
	}
	return out
}

func (s *scriptedServer) requests(cmd string) []*session.ExtensionMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*session.ExtensionMessage
	for _, m := range s.reqs {
		if m.Cmd == cmd {
			out = append(out, m)
		}
	}
	return out
}

func obj(kv ...any) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		switch v := kv[i+1].(type) {
		case int:
			o.PutInt(k, int32(v))
		case int32:
			o.PutInt(k, v)
		case int64:
			o.PutLong(k, v)
		case float64:
			o.PutDouble(k, v)
		case string:
			o.PutUtfString(k, v)
		case *sfs.SFSObject:
			o.PutSFSObject(k, v)
		case []*sfs.SFSObject:
			arr := sfs.NewSFSArray()
			for _, e := range v {
				arr.AddSFSObject(e)
			}
			o.PutSFSArray(k, arr)
		default:
			panic("obj: unsupported value type")
		}
	}
	return o
}

// productionBuilding is a building_new entry with a running production line.
func productionBuilding(uuid int64, bId, lv int32, prodST, prodT, prodET time.Time) Building {
	b := NewTestBuildingSFS(uuid, bId, lv)
	b.PutLong("prodST", prodST.UnixMilli())
	b.PutLong("prodT", prodT.UnixMilli())
	b.PutLong("prodET", prodET.UnixMilli())
	return Building{Raw: b}
}

// TestCollectCoreWithInitAppliesEveryGate runs the whole core sequence with an init push and
// checks each gate changes exactly what goes on the wire: al.show.help lists only the player's own
// request (no al.help.all), allianceNewMail is 0 (no gift requests at all), the VIP flags allow only
// the login score, the Smelter's 30-minute tick has not completed, and the Season 6 week-card farm
// is past seasonSettleTime -- so only the Farmland and the Component Factory (stored output) are
// collected.
func TestCollectCoreWithInitAppliesEveryGate(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	now := time.Now()

	raw := obj(
		"tomorrow", now.Add(time.Hour).Unix(),
		"allianceNewMail", 0,
		"user", obj("uid", "1001"),
		"vip", obj("vipInfo", obj("loginScoreState", 1, "everyDayReward", 0, "endTime", now.Add(24*time.Hour).Unix())),
		"playerServerSeasonInfo", obj("seasonSettleTime", now.Add(-time.Hour).UnixMilli()),
	)
	farm := productionBuilding(11, BuildingFarmland, 5, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	smelter := productionBuilding(12, BuildingSmelter, 5, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(time.Hour))
	weekCard := productionBuilding(13, BuildingSporeFactoryCard, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	component := productionBuilding(14, BuildingComponentFactory, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	component.Raw.PutInt("prodExtend", 1)
	in := &Init{Raw: raw, Buildings: []Building{farm, smelter, weekCard, component}}

	srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
		switch msg.Cmd {
		case "chat.get.system.mails":
			return "push.chat.get.system.mails", nil
		case "al.show.help":
			return "", obj("helpArr", []*sfs.SFSObject{obj("helpId", "h1", "senderId", "1001", "nowcount", 0, "maxcount", 5)})
		case "building.production.collect":
			return "", obj("status", 1)
		}
		return "", nil
	})

	if err := collectCore(client, in); err != nil {
		t.Fatalf("collectCore() = %v, want nil", err)
	}
	want := []string{
		"lw.pve.idle.reward", "lw.pve.idle.reward",
		"chat.get.system.mails",
		"al.show.help",
		"science.data.refresh",
		"vip.add.login.score",
		"building.production.collect", "building.production.collect",
	}
	if got := srv.cmds(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
	var collected []int64
	for _, m := range srv.requests("building.production.collect") {
		collected = append(collected, m.Params.GetLong("uuid"))
	}
	if !slices.Equal(collected, []int64{11, 14}) {
		t.Errorf("collected uuids = %v, want [11 14] (Farmland ready, Component Factory has stored output)", collected)
	}
}

// TestCollectAllWithoutInitKeepsUngatedSequence pins the in.Raw == nil path: the same buildings
// and no init means no al.show.help, alliance.reward.list or vip.info, both gift types and both VIP
// claims, and every building with prodST collected, even one whose tick has not completed.
func TestCollectAllWithoutInitKeepsUngatedSequence(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	now := time.Now()
	smelter := productionBuilding(12, BuildingSmelter, 5, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(time.Hour))
	weekCard := productionBuilding(13, BuildingSporeFactoryCard, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))

	srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
		if msg.Cmd == "chat.get.system.mails" {
			return "push.chat.get.system.mails", nil
		}
		return "", nil
	})

	if err := CollectAll(client, []Building{smelter, weekCard}, nil); err != nil {
		t.Fatalf("CollectAll() = %v, want nil", err)
	}
	want := []string{
		"lw.pve.idle.reward", "lw.pve.idle.reward",
		"chat.get.system.mails",
		"al.help.all",
		"alliance.reward.allreceive", "alliance.reward.allreceive",
		"science.data.refresh",
		"vip.add.login.score", "vip.get.every.day.reward",
		"building.production.collect", "building.production.collect",
	}
	if got := srv.cmds(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}
