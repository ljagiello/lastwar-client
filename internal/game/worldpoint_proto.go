package game

import (
	"errors"
	"fmt"
)

// A minimal protobuf wire reader for the world-tile blobs world.get.block returns (points[] and
// alInfos[], each one serialized protobuf.WorldPointInfo; WorldPointManager.ParseWorldGetBlock,
// Assembly-CSharp.decompiled.cs:180003-180060). go.mod has no protobuf module, and only these
// fields of WorldPointInfo.proto (1.0.364 assets/proto) are read; everything else is skipped by
// wire type:
//
//	WorldPointInfo { 1 id:int32 (tile index), 3 buildInfo:BuildInfo, 102 serverId:int32 }   (proto:429-460)
//	BuildInfo      { 1 ownerUid:string, 2 uuid:int64, 3 buildId:int32, 7 allianceId:string,
//	                 53 fireworks: repeated FireWorksInfo, 54 fireWorksGiftList: repeated FireWorksGift } (proto:3-60)
//	FireWorksGift  { 1 uuid:int64, 2 configId:int32, 3 num:int32, 4 max:int32, 5 sendTime:int64 (ms),
//	                 6 sendUid:string, 10 index:int32 }                                      (proto:707-715)
//
// Field 54 is tag 434 (bytes B2 03), field 53 tag 426 (AA 03). Malformed input returns an error,
// never a panic: the bytes come from the network.

const (
	pbVarint  = 0
	pbFixed64 = 1
	pbBytes   = 2
	pbFixed32 = 5
)

var errPBTruncated = errors.New("protobuf: truncated input")

// pbReader walks one message's fields.
type pbReader struct {
	b   []byte
	off int
}

func (r *pbReader) more() bool { return r.off < len(r.b) }

// varint reads a base-128 varint of at most 10 bytes.
func (r *pbReader) varint() (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		if r.off >= len(r.b) {
			return 0, errPBTruncated
		}
		c := r.b[r.off]
		r.off++
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("protobuf: varint longer than 10 bytes")
}

// tag reads a field key: the field number and the wire type.
func (r *pbReader) tag() (field uint64, wire int, err error) {
	k, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	field, wire = k>>3, int(k&7)
	if field == 0 {
		return 0, 0, errors.New("protobuf: field number 0")
	}
	return field, wire, nil
}

// bytes reads a length-delimited payload; the returned slice aliases the input.
func (r *pbReader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)-r.off) {
		return nil, errPBTruncated
	}
	out := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return out, nil
}

// skip passes over one value of the given wire type. Groups (3, 4) are not proto3 and are rejected.
func (r *pbReader) skip(wire int) error {
	var n int
	switch wire {
	case pbVarint:
		_, err := r.varint()
		return err
	case pbBytes:
		_, err := r.bytes()
		return err
	case pbFixed64:
		n = 8
	case pbFixed32:
		n = 4
	default:
		return fmt.Errorf("protobuf: unsupported wire type %d", wire)
	}
	if n > len(r.b)-r.off {
		return errPBTruncated
	}
	r.off += n
	return nil
}

// want checks that a known field arrived with the wire type its .proto declares.
func pbWant(field uint64, got, want int) error {
	if got != want {
		return fmt.Errorf("protobuf: field %d has wire type %d, want %d", field, got, want)
	}
	return nil
}

// worldPoint is the part of a WorldPointInfo the fireworks scan reads. Build is nil for a point
// without buildInfo (resources, monsters, ...).
type worldPoint struct {
	ID       int32
	ServerID int32
	Build    *worldBuild
}

// worldBuild is the part of a BuildInfo the fireworks scan reads. Shows counts field 53 (the
// firework shows playing over the base); Gifts is field 54.
type worldBuild struct {
	OwnerUID   string
	UUID       int64
	BuildID    int32
	AllianceID string
	Shows      int
	Gifts      []fireworkGift
}

// fireworkGift is one FireWorksGift: a claimable firework chest on the building.
type fireworkGift struct {
	UUID     int64
	ConfigID int32
	Num      int32
	Max      int32
	SendTime int64
	SendUID  string
	Index    int32
}

func decodeWorldPoint(b []byte) (worldPoint, error) {
	var p worldPoint
	r := &pbReader{b: b}
	for r.more() {
		field, wire, err := r.tag()
		if err != nil {
			return p, err
		}
		switch field {
		case 1, 102:
			if err := pbWant(field, wire, pbVarint); err != nil {
				return p, err
			}
			v, err := r.varint()
			if err != nil {
				return p, err
			}
			if field == 1 {
				p.ID = int32(v)
			} else {
				p.ServerID = int32(v)
			}
		case 3:
			if err := pbWant(field, wire, pbBytes); err != nil {
				return p, err
			}
			msg, err := r.bytes()
			if err != nil {
				return p, err
			}
			// A repeated occurrence of a message field merges into the first (protobuf semantics).
			if p.Build == nil {
				p.Build = &worldBuild{}
			}
			if err := decodeBuildInfo(msg, p.Build); err != nil {
				return p, fmt.Errorf("buildInfo: %w", err)
			}
		default:
			if err := r.skip(wire); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

func decodeBuildInfo(b []byte, bi *worldBuild) error {
	r := &pbReader{b: b}
	for r.more() {
		field, wire, err := r.tag()
		if err != nil {
			return err
		}
		switch field {
		case 1, 7, 53, 54:
			if err := pbWant(field, wire, pbBytes); err != nil {
				return err
			}
			v, err := r.bytes()
			if err != nil {
				return err
			}
			switch field {
			case 1:
				bi.OwnerUID = string(v)
			case 7:
				bi.AllianceID = string(v)
			case 53:
				bi.Shows++
			case 54:
				g, err := decodeFireworkGift(v)
				if err != nil {
					return fmt.Errorf("fireWorksGiftList: %w", err)
				}
				bi.Gifts = append(bi.Gifts, g)
			}
		case 2, 3:
			if err := pbWant(field, wire, pbVarint); err != nil {
				return err
			}
			v, err := r.varint()
			if err != nil {
				return err
			}
			if field == 2 {
				bi.UUID = int64(v)
			} else {
				bi.BuildID = int32(v)
			}
		default:
			if err := r.skip(wire); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeFireworkGift(b []byte) (fireworkGift, error) {
	var g fireworkGift
	r := &pbReader{b: b}
	for r.more() {
		field, wire, err := r.tag()
		if err != nil {
			return g, err
		}
		switch field {
		case 1, 2, 3, 4, 5, 10:
			if err := pbWant(field, wire, pbVarint); err != nil {
				return g, err
			}
			v, err := r.varint()
			if err != nil {
				return g, err
			}
			switch field {
			case 1:
				g.UUID = int64(v)
			case 2:
				g.ConfigID = int32(v)
			case 3:
				g.Num = int32(v)
			case 4:
				g.Max = int32(v)
			case 5:
				g.SendTime = int64(v)
			case 10:
				g.Index = int32(v)
			}
		case 6:
			if err := pbWant(field, wire, pbBytes); err != nil {
				return g, err
			}
			v, err := r.bytes()
			if err != nil {
				return g, err
			}
			g.SendUID = string(v)
		default:
			if err := r.skip(wire); err != nil {
				return g, err
			}
		}
	}
	return g, nil
}
