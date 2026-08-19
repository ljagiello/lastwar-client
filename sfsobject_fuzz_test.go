package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"testing"
)

// This file adds native Go fuzz targets for the two wire-format entry points that parse untrusted
// bytes coming straight off a real TCP connection to the game server (or, via decode.go's
// -decode-stream tool, from a captured file): DecodeObject (sfsobject.go) and ReadPacket
// (packet.go). Neither had any fuzz coverage before this -- only the hand-written edge cases in
// packet_oom_test.go, packet_bigsized_test.go, and sfsobject_array_test.go. Both targets assert
// only the one property fuzzing is meant to guard here: the function must never panic on malformed
// input. A returned error is fine and expected -- these decoders already reject plenty of hostile
// shapes (negative counts, excessive nesting, oversized frames) by design; only an unrecovered
// panic is a bug worth this test failing on.
//
// FuzzXxx functions also run as ordinary tests over just their seed corpus under plain
// `go test`/`go test ./...`, so the seeds below are exercised on every normal CI run even without
// the dedicated `-fuzz=` CI step (see .github/workflows/ci.yml) that extends the search further.

// sampleSFSObjects builds a handful of representative SFSObjects covering every field-type family
// this protocol uses (primitives, a nested SFSObject, a nested SFSArray containing a further
// nested object, and every primitive array/text wire type), so the fuzz seed corpus starts from
// real, well-formed encoder output rather than only hand-built hostile bytes.
func sampleSFSObjects() []*SFSObject {
	primitives := NewSFSObject()
	primitives.PutUtfString("cmd", "login")
	primitives.PutInt("uid", 42)
	primitives.PutBool("ok", true)
	primitives.PutByte("flag", 7)
	primitives.PutShort("level", 3)
	primitives.PutLong("ts", 1700000000000)
	primitives.PutDouble("ratio", 3.14159)

	nested := NewSFSObject()
	inner := NewSFSObject()
	inner.PutUtfString("loginKey", "not-a-real-secret")
	inner.PutInt("gameUid", 99)
	nested.PutSFSObject("accountArr", inner)
	nested.PutUtfString("cmd", "account.login")

	withArray := NewSFSObject()
	arr := NewSFSArray()
	arr.AddInt(1)
	arr.AddInt(2)
	arrObj := NewSFSObject()
	arrObj.PutUtfString("name", "building1")
	arrObj.PutInt("level", 5)
	arr.AddSFSObject(arrObj)
	withArray.PutSFSArray("defaultBuilds", arr)
	withArray.PutUtfString("cmd", "buildings.list")

	primitiveArrays := NewSFSObject()
	primitiveArrays.put("boolArr", SFSValue{sfsBoolArray, []bool{true, false, true}})
	primitiveArrays.put("byteArr", SFSValue{sfsByteArray, []byte{1, 2, 3, 4}})
	primitiveArrays.put("shortArr", SFSValue{sfsShortArray, []int16{-2, 7}})
	primitiveArrays.put("intArr", SFSValue{sfsIntArray, []int32{-100, 200}})
	primitiveArrays.put("longArr", SFSValue{sfsLongArray, []int64{1234567890123}})
	primitiveArrays.put("floatArr", SFSValue{sfsFloatArray, []float32{1.5}})
	primitiveArrays.put("doubleArr", SFSValue{sfsDoubleArray, []float64{2.5}})
	primitiveArrays.put("stringArr", SFSValue{sfsUtfStringArray, []string{"a", "b"}})
	primitiveArrays.put("text", SFSValue{sfsText, "a long-form text field"})
	primitiveArrays.put("nullField", SFSValue{sfsNull, nil})

	return []*SFSObject{primitives, nested, withArray, primitiveArrays}
}

// wrapSingleFieldObject builds a complete top-level SFSObject wire encoding (tag 18 + a 1-key
// header) whose single field's tagged value is exactly fieldTagAndPayload. DecodeObject requires
// that full envelope, unlike sfsobject_array_test.go's hostile-input tests, which call
// sfsReader.readValuePayload directly on a bare tagged-value payload -- this glues those same
// byte shapes into something DecodeObject itself will actually accept as input.
func wrapSingleFieldObject(key string, fieldTagAndPayload []byte) []byte {
	var buf []byte
	buf = append(buf, sfsObjectType)
	buf = binary.BigEndian.AppendUint16(buf, 1) // 1 key
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(key)))
	buf = append(buf, key...)
	buf = append(buf, fieldTagAndPayload...)
	return buf
}

// seedDeepNestBomb mirrors sfsobject_array_test.go's TestNestingDepthRejected: a chain of nested
// SFSArrays well past maxNestDepth, wrapped as a full DecodeObject input.
func seedDeepNestBomb() []byte {
	const levels = 200
	var val []byte
	val = append(val, sfsArrayType)
	val = binary.BigEndian.AppendUint16(val, 1) // count = 1
	for i := 0; i < levels-1; i++ {
		val = append(val, sfsArrayType)
		val = binary.BigEndian.AppendUint16(val, 1)
	}
	val = append(val, sfsBool, 1)
	return wrapSingleFieldObject("bomb", val)
}

// seedWideFanoutBomb mirrors sfsobject_array_test.go's TestDecodedNodeCountRejected: a shallow but
// wide-fan-out nested array whose total leaf count crosses maxDecodedNodes, wrapped as a full
// DecodeObject input.
func seedWideFanoutBomb() []byte {
	const outerCount = 10
	const innerCount = 30001 // outerCount * innerCount > maxDecodedNodes (300_000)

	var val []byte
	val = append(val, sfsArrayType)
	val = binary.BigEndian.AppendUint16(val, outerCount)
	for i := 0; i < outerCount; i++ {
		val = append(val, sfsArrayType)
		val = binary.BigEndian.AppendUint16(val, innerCount)
		for j := 0; j < innerCount; j++ {
			val = append(val, sfsNull)
		}
	}
	return wrapSingleFieldObject("bomb", val)
}

// seedNegativeArrayCount mirrors sfsobject_array_test.go's TestNestedCountRejectsNegative
// (SFSArray case), wrapped as a full DecodeObject input.
func seedNegativeArrayCount() []byte {
	val := []byte{sfsArrayType, 0xFF, 0xFF} // count = -1
	return wrapSingleFieldObject("bomb", val)
}

// seedNegativeByteArrayCount mirrors sfsobject_array_test.go's TestByteArrayRejectsNegativeCount,
// wrapped as a full DecodeObject input.
func seedNegativeByteArrayCount() []byte {
	var val []byte
	val = append(val, sfsByteArray)
	negOne := int32(-1)
	val = binary.BigEndian.AppendUint32(val, uint32(negOne))
	return wrapSingleFieldObject("bomb", val)
}

// seedTrailingGarbage mirrors sfsobject_array_test.go's TestDecodeObjectRejectsTrailingBytes: a
// well-formed encoded object with extra bytes appended after it, which DecodeObject must reject
// with an error, not a panic.
func seedTrailingGarbage() []byte {
	o := NewSFSObject()
	o.PutUtfString("key", "value")
	encoded, _ := EncodeObject(o) // cannot fail: trivially within every size limit
	return append(append([]byte(nil), encoded...), 0xDE, 0xAD, 0xBE)
}

// FuzzDecodeObject fuzzes DecodeObject, the top-level entry point for parsing a decoded SFS2X
// packet body (used by conn.go on every inbound server message, and by decode.go's
// -decode-stream tool on captured files). The only property asserted is "never panics" -- see the
// file-level doc comment above.
func FuzzDecodeObject(f *testing.F) {
	for _, o := range sampleSFSObjects() {
		data, err := EncodeObject(o)
		if err != nil {
			f.Fatalf("EncodeObject: %v", err)
		}
		f.Add(data)
	}

	f.Add(seedDeepNestBomb())
	f.Add(seedWideFanoutBomb())
	f.Add(seedNegativeArrayCount())
	f.Add(seedNegativeByteArrayCount())
	f.Add(seedTrailingGarbage())

	// A handful of trivially-malformed inputs a real corrupted/truncated frame could produce.
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte{sfsObjectType})
	f.Add([]byte{sfsNull})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeObject(data)
	})
}

// seedOversizedDeclaredLengthHeader mirrors packet_oom_test.go's
// TestReadPacketRejectsOversizedDeclaredLength: a header-only frame declaring a body length over
// maxFrameSize, which ReadPacket must reject with an error before attempting to read or allocate
// the (here, nonexistent) body.
func seedOversizedDeclaredLengthHeader() []byte {
	var hdr bytes.Buffer
	hdr.WriteByte(hdrBinary | hdrEncrypted | hdrBigSized)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], maxFrameSize+1)
	hdr.Write(lb[:])
	return hdr.Bytes()
}

// seedOversizedZstdUncompressedLengthHeader mirrors packet_oom_test.go's
// TestReadPacketRejectsOversizedZstdUncompressedLength: a frame whose zstd-flagged
// uncompressed-length field exceeds maxFrameSize, which ReadPacket must reject before attempting
// decompression.
func seedOversizedZstdUncompressedLengthHeader() []byte {
	var hdr bytes.Buffer
	hdr.WriteByte(hdrBinary | hdrEncrypted | hdrCompressed | hdrUseLZ4)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], 16) // small, otherwise-valid declared compressed length
	hdr.Write(lb[:])
	var ub [4]byte
	binary.BigEndian.PutUint32(ub[:], maxFrameSize+1)
	hdr.Write(ub[:])
	return hdr.Bytes()
}

// seedCompressedPacket builds a real EncodePacket(EncodeObject(...)) frame whose body is large
// enough (>compressionThreshold) and repetitive enough to reliably exercise EncodePacket's zlib
// branch, so the fuzz corpus includes a genuine compressed-frame shape for ReadPacket's
// decompression path, not just uncompressed ones.
func seedCompressedPacket(f *testing.F) []byte {
	o := NewSFSObject()
	o.PutUtfString("cmd", "buildings.list")
	o.PutUtfString("filler", strings.Repeat("last-war-client fuzz seed filler ", 100))
	body, err := EncodeObject(o)
	if err != nil {
		f.Fatalf("EncodeObject: %v", err)
	}
	packet, err := EncodePacket(body)
	if err != nil {
		f.Fatalf("EncodePacket: %v", err)
	}
	if packet[0]&hdrCompressed == 0 {
		f.Fatalf("seed setup bug: expected this packet to hit EncodePacket's compressed branch")
	}
	return packet
}

// seedBigSizedPacket builds a real EncodePacket(EncodeObject(...)) frame whose body is large and
// incompressible enough to push the compressed payload size over 65535 bytes, forcing
// EncodePacket's bigSized (4-byte length prefix) branch -- mirrors packet_bigsized_test.go's
// TestPacketRoundTripBigSized technique, but through a real EncodeObject-produced body (a byte
// array field) instead of a bare random byte slice standing in for one.
func seedBigSizedPacket(f *testing.F) []byte {
	raw := make([]byte, 70000)
	if _, err := rand.Read(raw); err != nil {
		f.Fatalf("rand.Read: %v", err)
	}
	o := NewSFSObject()
	o.put("blob", SFSValue{sfsByteArray, raw})
	body, err := EncodeObject(o)
	if err != nil {
		f.Fatalf("EncodeObject: %v", err)
	}
	packet, err := EncodePacket(body)
	if err != nil {
		f.Fatalf("EncodePacket: %v", err)
	}
	if packet[0]&hdrBigSized == 0 {
		f.Fatalf("seed setup bug: expected this packet to hit EncodePacket's bigSized branch")
	}
	return packet
}

// FuzzReadPacket fuzzes ReadPacket, the framing layer that reads one length-prefixed, optionally
// XOR-"encrypted" and zlib/zstd-compressed packet off a real io.Reader (conn.go's live TCP
// connection, or decode.go's -decode-stream tool on a captured file) before DecodeObject ever
// sees the bytes. The only property asserted is "never panics" -- see the file-level doc comment
// above.
func FuzzReadPacket(f *testing.F) {
	for _, o := range sampleSFSObjects() {
		body, err := EncodeObject(o)
		if err != nil {
			f.Fatalf("EncodeObject: %v", err)
		}
		packet, err := EncodePacket(body)
		if err != nil {
			f.Fatalf("EncodePacket: %v", err)
		}
		f.Add(packet)
	}

	f.Add(seedCompressedPacket(f))
	f.Add(seedBigSizedPacket(f))
	f.Add(seedOversizedDeclaredLengthHeader())
	f.Add(seedOversizedZstdUncompressedLengthHeader())

	// A handful of trivially-malformed/truncated inputs a corrupted or torn TCP read could
	// produce.
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte{hdrBinary | hdrEncrypted})
	f.Add([]byte{hdrBinary | hdrEncrypted | hdrForward})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadPacket(bytes.NewReader(data))
	})
}
