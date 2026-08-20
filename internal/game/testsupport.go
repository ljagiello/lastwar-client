package game

import "lastwar-client/internal/sfs"

// NewTestBuildingSFS builds a minimal building_new-shaped sfs.SFSObject with the fields Building's
// accessors read -- uuid/bId/lv -- mirroring what a real init push carries (see ParseInitBuildings'
// doc comment in buildings.go).
func NewTestBuildingSFS(uuid int64, bId, lv int32) *sfs.SFSObject {
	b := sfs.NewSFSObject()
	b.PutLong("uuid", uuid)
	b.PutInt("bId", bId)
	b.PutInt("lv", lv)
	return b
}

func NewTestBuilding(uuid int64, bId, lv int32) Building {
	return Building{Raw: NewTestBuildingSFS(uuid, bId, lv)}
}

// NewTestVisitor builds a Visitor whose Raw carries just the fields GreetVisitors and its own
// logging touch: uid, eventId, visitorId -- mirroring the shape ParseInitVisitors produces from a
// real `init` push (see the Visitor doc comment in visitors.go).
func NewTestVisitor(uid int64, eventId, visitorId int32) Visitor {
	raw := sfs.NewSFSObject()
	raw.PutLong("uid", uid)
	raw.PutInt("eventId", eventId)
	raw.PutInt("visitorId", visitorId)
	return Visitor{Raw: raw}
}
