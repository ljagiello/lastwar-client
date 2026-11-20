package game

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// cheerEntry is one init collect_reward entry (CollectRewardData.lua:18-52).
func cheerEntry(uuid int64, typ int32, contentID string, expire time.Time) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutLong("uuid", uuid)
	e.PutInt("type", typ)
	e.PutUtfString("contentId", contentID)
	e.PutLong("expireTime", expire.UnixMilli())
	return e
}

func cheerInit(entries ...*sfs.SFSObject) *Init {
	in := evInit(30)
	arr := sfs.NewSFSArray()
	for _, e := range entries {
		arr.AddSFSObject(e)
	}
	in.Raw.PutSFSArray("collect_reward", arr)
	return in
}

// TestFlowerTrainCheerClaimsWhenACheerBoxIsListed is MASTER 1.0.370 G23: a listed (unexpired) init
// collect_reward entry of type 21 whose contentId names an ice_supplies type-7 row (180-187 in
// 40413) makes the panel send flower.train.cheer.box.batch with no params
// (UICollectRewardView.lua:176-197, 349-367; FlowerTrainCheerBoxBatchMessage.lua:4-6). Other
// supplies, other entry types and expired entries do not, nor one expiring this millisecond: the
// panel lists expireTime > now only (GetRewardListBySort, CollectRewardDataManager.lua:132-133).
func TestFlowerTrainCheerClaimsWhenACheerBoxIsListed(t *testing.T) {
	withEvNow(t, evTestNow)
	later, earlier := evTestNow.Add(time.Hour), evTestNow.Add(-time.Minute)
	for name, c := range map[string]struct {
		entries []*sfs.SFSObject
		want    []string
	}{
		"cheer box": {[]*sfs.SFSObject{
			cheerEntry(1, 1, "", later), // a gather entry
			cheerEntry(2, 21, "183|1203|9001", later),
		}, []string{flowerTrainCheerCmd}},
		"two cheer boxes, one batch": {[]*sfs.SFSObject{
			cheerEntry(2, 21, "180|1203|9001", later), cheerEntry(3, 21, "187|1203|9002", later),
		}, []string{flowerTrainCheerCmd}},
		"another supply type (121 is type 6)": {[]*sfs.SFSObject{cheerEntry(2, 21, "121|1203|9001", later)}, nil},
		"type 7 supply on another entry type": {[]*sfs.SFSObject{cheerEntry(2, 22, "183|1203|9001", later)}, nil},
		"expired cheer box":                   {[]*sfs.SFSObject{cheerEntry(2, 21, "183|1203|9001", earlier)}, nil},
		"cheer box expiring now":              {[]*sfs.SFSObject{cheerEntry(2, 21, "183|1203|9001", evTestNow)}, nil},
		"supply id newer than the table":      {[]*sfs.SFSObject{cheerEntry(2, 21, "190|1203|9001", later)}, nil},
		"no contentId":                        {[]*sfs.SFSObject{cheerEntry(2, 21, "", later)}, nil},
		"empty panel":                         {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
			if err := runFlowerTrainCheer(conn, cheerInit(c.entries...)); err != nil {
				t.Fatal(err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Fatalf("sent %v, want %v", got, c.want)
			}
			for _, r := range fake.requests() {
				if keys := session.ParamKeysWithoutID(r.Params); len(keys) != 0 {
					t.Errorf("%s carried params %v, want none", r.Cmd, keys)
				}
			}
		})
	}
}

// TestFlowerTrainCheerLogsTheReplyAndStopsOnError: the first reply is logged at Info with the boxes
// it was sent for; an errorCode is an error (no benign code is registered for a never-sent command);
// an unknown supply id is logged at Warn; no init sends nothing.
func TestFlowerTrainCheerLogsTheReplyAndStopsOnError(t *testing.T) {
	withEvNow(t, evTestNow)
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	in := cheerInit(cheerEntry(77, 21, "184|1203|9001", evTestNow.Add(time.Hour)),
		cheerEntry(78, 21, "999|1203|9002", evTestNow.Add(time.Hour)))
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runFlowerTrainCheer(conn, in); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=INFO msg=\"flower-train-cheer-boxes: first flower.train.cheer.box.batch reply\"") ||
		!strings.Contains(out, "uuids=[77]") {
		t.Errorf("first reply not logged at Info with the box uuids:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ids=[999]") {
		t.Errorf("unknown supply id not logged at Warn:\n%s", out)
	}

	conn, _ = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("120289") })
	if err := runFlowerTrainCheer(conn, in); err == nil || !strings.Contains(err.Error(), "120289") {
		t.Errorf("err = %v, want the 120289 refusal", err)
	}

	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runFlowerTrainCheer(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v", err, fake.cmds())
	}
}

// TestFlowerTrainCheerIsOptInAndRunsAfterCollectRewards: a new claim is opt-in and static-only, and
// it runs after collect-rewards (same phase, by name), as the client sends it after the gather claim.
func TestFlowerTrainCheerIsOptInAndRunsAfterCollectRewards(t *testing.T) {
	f, ok := featureRegistry[flowerTrainCheerFeature]
	if !ok {
		t.Fatal("flower-train-cheer-boxes is not registered")
	}
	if f.DefaultOn {
		t.Error("DefaultOn; a new claim must be opt-in until validated live")
	}
	if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("Summary = %q, want it to end with (static-only, not validated live)", f.Summary)
	}
	order := featuresInRunOrder()
	at := func(name string) int {
		return slices.IndexFunc(order, func(f Feature) bool { return f.Name == name })
	}
	if c, f := at("collect-rewards"), at(flowerTrainCheerFeature); c < 0 || f < 0 || f < c {
		t.Errorf("run order: collect-rewards at %d, %s at %d; want the cheer batch after the gather claim", c, flowerTrainCheerFeature, f)
	}
}

// TestFlowerTrainCheerSupplyTableHasTheCheerRows pins the embedded ice_supplies type column: 73
// rows in 40413, of which exactly 180-187 are type 7.
func TestFlowerTrainCheerSupplyTableHasTheCheerRows(t *testing.T) {
	if n := len(flowerTrainCheerSupplyTypes); n != 73 {
		t.Errorf("ice_supplies rows = %d, want 73 (tables 40413)", n)
	}
	var cheer []int64
	for id, typ := range flowerTrainCheerSupplyTypes {
		if typ == flowerTrainCheerSupplyType {
			cheer = append(cheer, id)
		}
	}
	slices.Sort(cheer)
	if want := []int64{180, 181, 182, 183, 184, 185, 186, 187}; !slices.Equal(cheer, want) {
		t.Errorf("type-7 supplies = %v, want %v", cheer, want)
	}
}
