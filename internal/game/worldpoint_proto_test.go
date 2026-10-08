package game

import (
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"testing"
)

// Test-side protobuf encoder for building WorldPointInfo fixtures. It writes plain proto3 wire
// format, so the hand-written byte fixtures below pin it to the .proto layout.

func pbAppendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbKey(b []byte, field, wire int) []byte { return pbAppendVarint(b, uint64(field<<3|wire)) }

func pbVarintField(b []byte, field int, v int64) []byte {
	return pbAppendVarint(pbKey(b, field, pbVarint), uint64(v))
}

func pbBytesField(b []byte, field int, v []byte) []byte {
	b = pbAppendVarint(pbKey(b, field, pbBytes), uint64(len(v)))
	return append(b, v...)
}

func pbGift(g fireworkGift) []byte {
	var b []byte
	b = pbVarintField(b, 1, g.UUID)
	b = pbVarintField(b, 2, int64(g.ConfigID))
	b = pbVarintField(b, 3, int64(g.Num))
	b = pbVarintField(b, 4, int64(g.Max))
	b = pbVarintField(b, 5, g.SendTime)
	if g.SendUID != "" {
		b = pbBytesField(b, 6, []byte(g.SendUID))
	}
	return pbVarintField(b, 10, int64(g.Index))
}

// fixtureGift is FireWorksGift{uuid 7000000000123, configId 661502, num 3, max 15,
// sendTime 1791212400000, sendUid "s1", index 1}, encoded by hand from proto:707-715.
var fixtureGift = []byte{
	0x08, 0xfb, 0xe0, 0x8d, 0x84, 0xdd, 0xcb, 0x01, // 1 uuid = 7000000000123
	0x10, 0xfe, 0xaf, 0x28, // 2 configId = 661502
	0x18, 0x03, // 3 num = 3
	0x20, 0x0f, // 4 max = 15
	0x28, 0x80, 0xdb, 0xd0, 0xe4, 0x90, 0x34, // 5 sendTime = 1791212400000
	0x32, 0x02, 's', '1', // 6 sendUid = "s1"
	0x50, 0x01, // 10 index = 1
}

var wantFixtureGift = fireworkGift{UUID: 7000000000123, ConfigID: 661502, Num: 3, Max: 15, SendTime: 1791212400000, SendUID: "s1", Index: 1}

// fixtureBuild is a BuildInfo with the read fields, two skipped ones (28 recoverSpeed, a fixed32
// float; 13 currentHp, a varint), one firework show (53) and fixtureGift (54).
func fixtureBuild() []byte {
	b := []byte{
		0x0a, 0x02, 'o', '1', // 1 ownerUid = "o1"
		0x10, 0x2a, // 2 uuid = 42
		0x18, 0xa0, 0xba, 0xe8, 0x04, // 3 buildId = 10100000
		0x3a, 0x03, 'a', 'l', '1', // 7 allianceId = "al1"
		0xe5, 0x01, 0x00, 0x00, 0xc0, 0x3f, // 28 recoverSpeed = 1.5 (fixed32)
		0x68, 0x05, // 13 currentHp = 5
		0xaa, 0x03, 0x07, 0x0a, 0x01, 'p', 0x48, 0xfe, 0xaf, 0x28, // 53 fireworks {pic "p", configId 661502}
		0xb2, 0x03, byte(len(fixtureGift)), // 54 fireWorksGiftList, tag 434
	}
	return append(b, fixtureGift...)
}

func fixturePoint() []byte {
	build := fixtureBuild()
	b := []byte{
		0x08, 0xf9, 0x9a, 0x15, // 1 id = 347513
		0x10, 0x01, // 2 pointType = 1
		0x1a, byte(len(build)), // 3 buildInfo
	}
	b = append(b, build...)
	return append(b,
		0xa0, 0x06, 0x63, // 100 uuid = 99
		0xaa, 0x06, 0x02, 0x01, 0x02, // 101 extraInfo = {1, 2}
		0xb0, 0x06, 0x8f, 0x06, // 102 serverId = 783
	)
}

func TestDecodeFireworkGiftFixture(t *testing.T) {
	g, err := decodeFireworkGift(fixtureGift)
	if err != nil || g != wantFixtureGift {
		t.Fatalf("decoded %+v, %v\nwant %+v", g, err, wantFixtureGift)
	}
	if enc := pbGift(wantFixtureGift); !slices.Equal(enc, fixtureGift) {
		t.Errorf("test encoder drifted from the hand fixture:\n% x\n% x", enc, fixtureGift)
	}
}

func TestDecodeWorldPointFixture(t *testing.T) {
	p, err := decodeWorldPoint(fixturePoint())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 347513 || p.PointType != 1 || p.UUID != 99 || p.ServerID != 783 || p.Build == nil || p.Treasure != nil {
		t.Fatalf("point = %+v", p)
	}
	b := p.Build
	if b.OwnerUID != "o1" || b.UUID != 42 || b.BuildID != claimHQBuildingID || b.AllianceID != "al1" || b.Shows != 1 {
		t.Errorf("build = %+v", b)
	}
	if len(b.Gifts) != 1 || b.Gifts[0] != wantFixtureGift {
		t.Errorf("gifts = %+v", b.Gifts)
	}
}

// fixtureTreasure is TreasurePointInfo{uuid 300, ownerUid "o2", eventId "25001", completionTime
// 1791212400000, allianceId "al1", rewardUserList ["u1", "u2"], complete true, expireTime
// 1791241200000, type 1, createTime 1791205200000, multiple 2}, encoded by hand from proto:603-624,
// with fields the reader skips: allianceAbbr (6), diggingUserList (8, a message), speed (11, a
// fixed32 float), ownerName (12), fromPoint (17) and killerId (19).
var fixtureTreasure = []byte{
	0x08, 0xac, 0x02, // 1 uuid = 300
	0x12, 0x02, 'o', '2', // 2 ownerUid = "o2"
	0x1a, 0x05, '2', '5', '0', '0', '1', // 3 eventId = "25001" (a string)
	0x20, 0x80, 0xdb, 0xd0, 0xe4, 0x90, 0x34, // 4 completionTime = 1791212400000
	0x2a, 0x03, 'a', 'l', '1', // 5 allianceId = "al1"
	0x32, 0x02, 'A', 'B', // 6 allianceAbbr = "AB"
	0x3a, 0x02, 'u', '1', // 7 rewardUserList += "u1"
	0x3a, 0x02, 'u', '2', // 7 rewardUserList += "u2"
	0x42, 0x03, 0x0a, 0x01, 'd', // 8 diggingUserList += {uid "d"}
	0x50, 0x01, // 10 complete = true
	0x5d, 0x00, 0x00, 0xc0, 0x3f, // 11 speed = 1.5 (fixed32)
	0x62, 0x01, 'n', // 12 ownerName = "n"
	0x68, 0x80, 0xc3, 0xae, 0xf2, 0x90, 0x34, // 13 expireTime = 1791241200000
	0x70, 0x01, // 14 type = 1 (RadarTreasure)
	0x78, 0x80, 0xa1, 0x99, 0xe1, 0x90, 0x34, // 15 createTime = 1791205200000
	0x88, 0x01, 0x05, // 17 fromPoint = 5
	0x90, 0x01, 0x02, // 18 multiple = 2
	0x9a, 0x01, 0x01, 'k', // 19 killerId = "k"
}

// fixtureTreasurePoint is WorldPointInfo{id 347540, pointType 21, treasurePointInfo, uuid
// 7000000000999, serverId 783}.
func fixtureTreasurePoint() []byte {
	b := []byte{
		0x08, 0x94, 0x9b, 0x15, // 1 id = 347540
		0x10, 0x15, // 2 pointType = 21 (TREASURE)
		0x5a, byte(len(fixtureTreasure)), // 11 treasurePointInfo
	}
	b = append(b, fixtureTreasure...)
	return append(b,
		0xa0, 0x06, 0xe7, 0xe7, 0x8d, 0x84, 0xdd, 0xcb, 0x01, // 100 uuid = 7000000000999
		0xb0, 0x06, 0x8f, 0x06, // 102 serverId = 783
	)
}

func TestDecodeTreasurePointFixture(t *testing.T) {
	p, err := decodeWorldPoint(fixtureTreasurePoint())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 347540 || p.PointType != 21 || p.UUID != 7000000000999 || p.ServerID != 783 || p.Build != nil || p.Treasure == nil {
		t.Fatalf("point = %+v", p)
	}
	tr := p.Treasure
	if tr.UUID != 300 || tr.OwnerUID != "o2" || tr.EventID != "25001" || tr.CompletionTime != 1791212400000 ||
		tr.AllianceID != "al1" || !tr.Complete || tr.ExpireTime != 1791241200000 || tr.Type != 1 ||
		tr.CreateTime != 1791205200000 || tr.Multiple != 2 {
		t.Errorf("treasure = %+v", tr)
	}
	if tr.Claimers() != 2 || !tr.ClaimedBy("u1") || !tr.ClaimedBy("u2") || tr.ClaimedBy("d") || tr.ClaimedBy("") {
		t.Errorf("claimers: %d, the digger and the empty uid are not claimers", tr.Claimers())
	}
	full := fixtureTreasurePoint()
	for n := range len(full) {
		if _, err := decodeWorldPoint(full[:n]); err == nil && n > 0 && !pbFieldBoundary(full, n) {
			t.Errorf("truncated to %d bytes decoded without error", n)
		}
	}
}

func TestDecodeTreasureRejectsWrongWireTypes(t *testing.T) {
	cases := map[string][]byte{
		"treasure as varint":   {0x58, 0x01},
		"pointType as bytes":   {0x12, 0x00},
		"uuid as bytes":        {0xa2, 0x06, 0x00},
		"eventId as varint":    pbBytesField(nil, 11, []byte{0x18, 0x01}),
		"claimer as varint":    pbBytesField(nil, 11, []byte{0x38, 0x01}),
		"truncated claimer":    pbBytesField(nil, 11, []byte{0x3a, 0x05, 'u'}),
		"expireTime as fixed":  pbBytesField(nil, 11, []byte{0x69, 0, 0, 0, 0, 0, 0, 0, 0}),
		"truncated digger msg": pbBytesField(nil, 11, []byte{0x42, 0x09, 0x0a}),
	}
	for name, b := range cases {
		_, err := decodeWorldPoint(b)
		if err == nil {
			t.Errorf("%s: decoded without error", name)
			continue
		}
		if b[0] == 0x5a && !strings.Contains(err.Error(), "treasurePointInfo") {
			t.Errorf("%s: err %v does not name treasurePointInfo", name, err)
		}
	}
}

func TestDecodeWorldPointWithoutBuildInfo(t *testing.T) {
	// A resource point: id, pointType 3, resourceInfo (6) only.
	p, err := decodeWorldPoint([]byte{0x08, 0x05, 0x10, 0x03, 0x32, 0x02, 0x08, 0x01})
	if err != nil || p.ID != 5 || p.Build != nil {
		t.Errorf("point = %+v, %v", p, err)
	}
}

func TestDecodeNegativeInt32AndRepeatedGifts(t *testing.T) {
	var bi []byte
	bi = pbBytesField(bi, 54, pbGift(fireworkGift{UUID: 1, ConfigID: 671001, Num: -1, Max: 100}))
	bi = pbBytesField(bi, 54, pbGift(fireworkGift{UUID: 2, ConfigID: 661501, Num: 0, Max: 10}))
	if !slices.Equal(pbVarintField(nil, 3, -1), []byte{0x18, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}) {
		t.Fatal("a negative int32 is a 10-byte sign-extended varint")
	}
	var b worldBuild
	if err := decodeBuildInfo(bi, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Gifts) != 2 || b.Gifts[0].Num != -1 || b.Gifts[0].ConfigID != 671001 || b.Gifts[1].UUID != 2 {
		t.Errorf("gifts = %+v", b.Gifts)
	}
}

func TestDecodeWorldPointSkipsFixed64(t *testing.T) {
	var p []byte
	p = pbVarintField(p, 1, 9)
	p = pbKey(p, 50, pbFixed64)
	p = binary.LittleEndian.AppendUint64(p, math.Float64bits(2.5))
	p = pbVarintField(p, 102, 4)
	got, err := decodeWorldPoint(p)
	if err != nil || got.ID != 9 || got.ServerID != 4 {
		t.Errorf("point = %+v, %v", got, err)
	}
}

func TestDecodeWorldPointRejectsMalformedInput(t *testing.T) {
	full := fixturePoint()
	for n := range len(full) {
		if _, err := decodeWorldPoint(full[:n]); err == nil && n > 0 {
			// Only a cut at a field boundary may decode; every boundary of fixturePoint is a
			// complete field, so a cut inside a field must fail.
			if !pbFieldBoundary(full, n) {
				t.Errorf("truncated to %d bytes decoded without error", n)
			}
		}
	}
	cases := map[string][]byte{
		"group wire type":         {0x0b},
		"field number 0":          {0x00, 0x01},
		"overlong varint":         {0x08, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01},
		"length past end":         {0x1a, 0x05, 0x0a},
		"buildInfo as varint":     {0x18, 0x01},
		"id as bytes":             {0x0a, 0x00},
		"gift list wrong type":    pbBytesField(nil, 3, []byte{0xb0, 0x03, 0x01}),
		"truncated fixed32":       {0x95, 0x03, 0x00, 0x00}, // field 50, wire type 5, 2 of 4 bytes
		"truncated gift in build": pbBytesField(nil, 3, pbBytesField(nil, 54, []byte{0x08})),
	}
	for name, b := range cases {
		if _, err := decodeWorldPoint(b); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
	if _, err := decodeWorldPoint(nil); err != nil {
		t.Errorf("an empty message is valid: %v", err)
	}
}

// pbFieldBoundary reports whether n is the offset of a top-level field boundary in b.
func pbFieldBoundary(b []byte, n int) bool {
	r := &pbReader{b: b}
	for r.more() {
		if r.off == n {
			return true
		}
		_, wire, err := r.tag()
		if err != nil || r.skip(wire) != nil {
			return false
		}
	}
	return r.off == n
}

func TestPBErrorsNameTheField(t *testing.T) {
	_, err := decodeWorldPoint(pbBytesField(nil, 3, pbBytesField(nil, 54, []byte{0x12, 0x00})))
	if err == nil || !strings.Contains(err.Error(), "fireWorksGiftList") || !strings.Contains(err.Error(), "field 2") {
		t.Errorf("err = %v", err)
	}
}
