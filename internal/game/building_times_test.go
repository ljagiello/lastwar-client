package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// buildQueueEntry is an init queue_new entry; itemID is put as a string, an int64 as a Long, nil
// leaves itemObj out, and uT < 0 leaves the uT key out.
func buildQueueEntry(qType int32, itemID any, uT int64) *sfs.SFSObject {
	q := sfs.NewSFSObject()
	q.PutLong("uuid", 900+int64(qType))
	q.PutInt("type", qType)
	if uT >= 0 {
		q.PutLong("uT", uT)
	}
	switch id := itemID.(type) {
	case string:
		item := sfs.NewSFSObject()
		item.PutUtfString("itemId", id)
		q.PutSFSObject("itemObj", item)
	case int64:
		item := sfs.NewSFSObject()
		item.PutLong("itemId", id)
		q.PutSFSObject("itemObj", item)
	}
	return q
}

func TestBuildingUpgradeTime(t *testing.T) {
	const own = int64(1_800_000_000_000)
	building := func(uuid int64) *sfs.SFSObject {
		b := NewTestBuildingSFS(uuid, BuildingIronMine, 5)
		b.PutLong("uT", own)
		return b
	}
	cases := []struct {
		name       string
		queue      *sfs.SFSObject
		want       int64
		overridden bool
	}{
		{"normal queue, itemId string", buildQueueEntry(0, "11", own+60_000), own + 60_000, true},
		{"special queue, itemId Long", buildQueueEntry(110, int64(11), own-60_000), own - 60_000, true},
		{"uT 0 still overrides (Lua truthiness)", buildQueueEntry(0, "11", 0), 0, true},
		{"queue without uT", buildQueueEntry(0, "11", -1), own, false},
		{"another building's queue", buildQueueEntry(0, "12", own+60_000), own, false},
		{"idle queue, no itemObj", buildQueueEntry(0, nil, own+60_000), own, false},
		{"research queue naming the building", buildQueueEntry(6, "11", own+60_000), own, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := sfs.NewSFSObject()
			qs := sfs.NewSFSArray()
			qs.AddSFSObject(c.queue)
			raw.PutSFSArray("queue_new", qs)
			got, overridden := buildingUpgradeTime(&Init{Raw: raw}, building(11))
			if got != c.want || overridden != c.overridden {
				t.Errorf("buildingUpgradeTime = (%d, %v), want (%d, %v)", got, overridden, c.want, c.overridden)
			}
		})
	}
	if got, overridden := buildingUpgradeTime(&Init{Raw: sfs.NewSFSObject()}, building(11)); got != own || overridden {
		t.Errorf("no queue_new: (%d, %v), want (%d, false)", got, overridden, own)
	}
}
