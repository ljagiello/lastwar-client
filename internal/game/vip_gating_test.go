package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// TestClaimVIPDailiesGating covers claimVIPDailies' decisions end to end over a fake server: which
// of vip.info, vip.add.login.score and vip.get.every.day.reward go out for each init shape.
func TestClaimVIPDailiesGating(t *testing.T) {
	now := time.Now()
	active := now.Add(24 * time.Hour).Unix()
	expired := now.Add(-time.Hour).Unix()
	fresh := now.Add(time.Hour).Unix() // init's next server-day reset is still ahead
	stale := now.Add(-time.Minute).Unix()
	vipInit := func(tomorrow int64, info *sfs.SFSObject) *Init {
		raw := obj("tomorrow", tomorrow)
		if info != nil {
			raw.PutSFSObject("vip", obj("vipInfo", info))
		}
		return &Init{Raw: raw}
	}
	flags := func(login, freebie int, endTime int64) *sfs.SFSObject {
		return obj("loginScoreState", login, "everyDayReward", freebie, "endTime", endTime)
	}
	const (
		login   = "vip.add.login.score"
		freebie = "vip.get.every.day.reward"
		info    = "vip.info"
	)
	cases := []struct {
		name      string
		in        *Init
		vipInfo   *sfs.SFSObject // vip.info reply; nil replies with an errorCode
		want      []string
		wantError bool
	}{
		{"no init: both, unconditionally", &Init{}, nil, []string{login, freebie}, false},
		{"both claimable, VIP active", vipInit(fresh, flags(1, 1, active)), nil, []string{login, freebie}, false},
		{"both already claimed", vipInit(fresh, flags(0, 0, active)), nil, nil, false},
		{"freebie needs active VIP", vipInit(fresh, flags(1, 1, expired)), nil, []string{login}, false},
		{"freebie needs an endTime", vipInit(fresh, obj("loginScoreState", 0, "everyDayReward", 1)), nil, nil, false},
		{"stale init: refreshed flags decide", vipInit(stale, flags(0, 0, active)), obj("vipInfo", flags(0, 1, active)), []string{info, freebie}, false},
		{"stale init, refresh fails: stale flags count as claimable", vipInit(stale, flags(0, 0, active)), nil, []string{info, login, freebie}, true},
		{"stale init, refresh fails, VIP expired", vipInit(stale, flags(0, 0, expired)), nil, []string{info, login}, true},
		{"no vipInfo in init: refreshed", vipInit(fresh, nil), obj("vipInfo", flags(1, 0, active)), []string{info, login}, false},
		{"no vipInfo anywhere: both", vipInit(fresh, nil), obj("other", 1), []string{info, login, freebie}, false},
		{"no tomorrow: init taken as fresh", &Init{Raw: obj("vip", obj("vipInfo", flags(1, 0, active)))}, nil, []string{login}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				if msg.Cmd == info {
					if c.vipInfo == nil {
						return "", obj("errorCode", "E500")
					}
					return "", c.vipInfo
				}
				return "", nil
			})
			err := claimVIPDailies(client, c.in, now)
			if (err != nil) != c.wantError {
				t.Errorf("claimVIPDailies() error = %v, want error %v", err, c.wantError)
			}
			if got := srv.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("requests = %v, want %v", got, c.want)
			}
		})
	}
}

// TestClaimVIPDailiesBenignAlreadyClaimed keeps 120289 benign on the gated path.
func TestClaimVIPDailiesBenignAlreadyClaimed(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	now := time.Now()
	in := &Init{Raw: obj("tomorrow", now.Add(time.Hour).Unix(),
		"vip", obj("vipInfo", obj("loginScoreState", 1, "everyDayReward", 1, "endTime", now.Add(time.Hour).Unix())))}
	srv := serveScripted(server, func(*session.ExtensionMessage) (string, *sfs.SFSObject) {
		return "", obj("errorCode", "120289")
	})
	if err := claimVIPDailies(client, in, now); err != nil {
		t.Errorf("claimVIPDailies() = %v, want nil (120289 = already claimed today)", err)
	}
	if got := srv.cmds(); len(got) != 2 {
		t.Errorf("requests = %v, want both claims", got)
	}
}
