package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// SFS2X SFSDataType tags, per the reverse-engineered wire format (see dossier §04).
const (
	sfsNull           = 0
	sfsBool           = 1
	sfsByte           = 2
	sfsShort          = 3
	sfsInt            = 4
	sfsLong           = 5
	sfsFloat          = 6
	sfsDouble         = 7
	sfsUtfString      = 8
	sfsBoolArray      = 9
	sfsByteArray      = 10
	sfsShortArray     = 11
	sfsIntArray       = 12
	sfsLongArray      = 13
	sfsFloatArray     = 14
	sfsDoubleArray    = 15
	sfsUtfStringArray = 16
	sfsArrayType      = 17
	sfsObjectType     = 18
	sfsClass          = 19 // unused/unimplemented by the game
	sfsText           = 20
)

// SFSValue is a single tagged field value.
type SFSValue struct {
	Type byte
	Val  interface{}
}

// SFSObject is an ordered string-keyed map, matching the client's own
// "insert order doesn't matter for lookup, but we preserve it for wire
// determinism" behavior.
type SFSObject struct {
	keys   []string
	values map[string]SFSValue
}

func NewSFSObject() *SFSObject {
	return &SFSObject{values: make(map[string]SFSValue)}
}

func (o *SFSObject) put(key string, v SFSValue) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

func (o *SFSObject) PutUtfString(key, val string)      { o.put(key, SFSValue{sfsUtfString, val}) }
func (o *SFSObject) PutBool(key string, val bool)      { o.put(key, SFSValue{sfsBool, val}) }
func (o *SFSObject) PutByte(key string, val byte)      { o.put(key, SFSValue{sfsByte, val}) }
func (o *SFSObject) PutShort(key string, val int16)    { o.put(key, SFSValue{sfsShort, val}) }
func (o *SFSObject) PutInt(key string, val int32)      { o.put(key, SFSValue{sfsInt, val}) }
func (o *SFSObject) PutLong(key string, val int64)     { o.put(key, SFSValue{sfsLong, val}) }
func (o *SFSObject) PutDouble(key string, val float64) { o.put(key, SFSValue{sfsDouble, val}) }
func (o *SFSObject) PutSFSObject(key string, val *SFSObject) {
	o.put(key, SFSValue{sfsObjectType, val})
}
func (o *SFSObject) PutSFSArray(key string, val *SFSArray) { o.put(key, SFSValue{sfsArrayType, val}) }

// Get helpers for decoded responses.
func (o *SFSObject) Has(key string) bool             { _, ok := o.values[key]; return ok }
func (o *SFSObject) Get(key string) (SFSValue, bool) { v, ok := o.values[key]; return v, ok }
func (o *SFSObject) GetString(key string) string {
	if v, ok := o.values[key]; ok {
		if s, ok := v.Val.(string); ok {
			return s
		}
	}
	return ""
}
func (o *SFSObject) GetInt(key string) int32 {
	if v, ok := o.values[key]; ok {
		switch n := v.Val.(type) {
		case int32:
			return n
		case int16:
			return int32(n)
		case byte:
			return int32(n)
		case int64:
			return int32(n)
		}
	}
	return 0
}
func (o *SFSObject) GetLong(key string) int64 {
	if v, ok := o.values[key]; ok {
		switch n := v.Val.(type) {
		case int64:
			return n
		case int32:
			return int64(n)
		case int16:
			return int64(n)
		case byte:
			return int64(n)
		}
	}
	return 0
}
func (o *SFSObject) String() string {
	var b bytes.Buffer
	b.WriteString("{")
	for i, k := range o.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v := o.values[k]
		fmt.Fprintf(&b, "%s=%s", k, formatSFSValue(v))
	}
	b.WriteString("}")
	return b.String()
}

// formatSFSValue recurses into nested SFSObject/SFSArray values instead of
// printing their Go pointer, so String() dumps are actually useful for
// inspecting arrays-of-objects like `accountArr`/`defaultBuilds`.
func formatSFSValue(v SFSValue) string {
	switch val := v.Val.(type) {
	case *SFSObject:
		return val.String()
	case *SFSArray:
		var b bytes.Buffer
		b.WriteString("[")
		for i, item := range val.items {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatSFSValue(item))
		}
		b.WriteString("]")
		return b.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

// sensitiveSFSKeys lists the field names this protocol is known to carry live credentials/tokens
// under, across every login/session response and request this repo decodes or builds (see
// login.go's redact() call sites, identity.go's BuildLoginParams, and gsl.go's
// LoginServerListRespon.At/Rt) -- loginKey/accountArr's sibling, gameUid, is deliberately not
// included: it identifies an account but isn't a bearer credential by itself.
var sensitiveSFSKeys = map[string]bool{
	"loginKey":    true,
	"at":          true,
	"rt":          true,
	"accessToken": true,
	"airKey":      true,
	"shumeiBoxId": true,
	"pw":          true,
	"password":    true,
	// verifyCode is the live one-time email-verification code account.login.new sends to
	// complete login (see login.go's finishParams.PutUtfString("verifyCode", code)).
	"verifyCode": true,
	// deviceId is, together with airKey (already above), the actual SFS-layer bearer credential
	// for the base zone Login (see login.go's/identity.go's BuildLoginParams doc comments) -- it
	// always appears alongside airKey in loginParams, so it must redact the same way.
	"deviceId": true,
	// chatToken is documented (docs/auth.mdx, docs/alliance-chat-mail.mdx) as a live bearer
	// credential for the separate chat WebSocket, carried in the `init` push's params. Not yet
	// consumed by any Go code (chat isn't implemented) -- added pre-emptively/defense-in-depth.
	"chatToken": true,
	// tk is the vanilla SFS2X Handshake response's session token -- docs/wire-protocol.mdx
	// documents a real captured Handshake response shape `{ct=3072, ms=1000000, tk=<32-hex>}`
	// from the live production server, explicitly calling tk a session token.
	"tk": true,
}

// StringRedacted is String()'s safe-to-log twin: a decoded server response or outgoing request
// can carry a live loginKey/accessToken/airKey/shumeiBoxId in cleartext (this protocol has no
// separate "credentials" envelope -- they're ordinary fields mixed in with gameplay data), and
// String()'s fully generic dump has no way to tell those fields apart from an ordinary uid or
// building level. StringRedacted walks the same structure but masks any key in sensitiveSFSKeys
// (recursing into nested SFSObject/SFSArray values the same way formatSFSValue does) instead of
// printing its value, so a call site that wants to log/error-wrap a full decoded object for
// debugging can do so without risking a credential leak.
func (o *SFSObject) StringRedacted() string {
	var b bytes.Buffer
	b.WriteString("{")
	for i, k := range o.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v := o.values[k]
		if sensitiveSFSKeys[k] {
			fmt.Fprintf(&b, "%s=%s", k, redactSFSValue(v))
		} else {
			fmt.Fprintf(&b, "%s=%s", k, formatSFSValueRedacted(v))
		}
	}
	b.WriteString("}")
	return b.String()
}

// formatSFSValueRedacted is formatSFSValue's redacted twin, used by StringRedacted.
func formatSFSValueRedacted(v SFSValue) string {
	switch val := v.Val.(type) {
	case *SFSObject:
		return val.StringRedacted()
	case *SFSArray:
		var b bytes.Buffer
		b.WriteString("[")
		for i, item := range val.items {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatSFSValueRedacted(item))
		}
		b.WriteString("]")
		return b.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

// redactSFSValue masks a sensitive-keyed field's value. Every known sensitive key
// (sensitiveSFSKeys) carries a plain string on the wire in every case this repo has decoded; a
// non-string value under one of those keys would be unexpected. A primitive array (one of the 8
// types readValuePayload's array-tag cases decode into -- []bool/[]byte/[]int16/[]int32/[]int64/
// []float32/[]float64/[]string) still gets masked explicitly below, since formatSFSValueRedacted's
// fallback for those types is the same naive fmt.Sprintf("%v", val) String() uses -- printing the
// raw slice contents with no redaction at all, defeating this function's whole point. Any other
// non-string, non-array shape falls back to the ordinary safe recursive formatter.
func redactSFSValue(v SFSValue) string {
	if s, ok := v.Val.(string); ok {
		return redact(s)
	}
	if n, ok := primitiveArrayLen(v.Val); ok {
		return fmt.Sprintf("[REDACTED %d items]", n)
	}
	return formatSFSValueRedacted(v)
}

// primitiveArrayLen reports the length of val and true if val is one of the 8 primitive array
// types readValuePayload's array-tag cases (sfsBoolArray..sfsUtfStringArray) decode into -- plain
// unwrapped Go slices, as opposed to the *SFSArray wrapper type. Used by redactSFSValue to mask a
// sensitive key's array value without dumping its raw contents.
func primitiveArrayLen(val interface{}) (int, bool) {
	switch a := val.(type) {
	case []bool:
		return len(a), true
	case []byte:
		return len(a), true
	case []int16:
		return len(a), true
	case []int32:
		return len(a), true
	case []int64:
		return len(a), true
	case []float32:
		return len(a), true
	case []float64:
		return len(a), true
	case []string:
		return len(a), true
	}
	return 0, false
}

// SFSArray is a sequential list of tagged values.
type SFSArray struct {
	items []SFSValue
}

func NewSFSArray() *SFSArray { return &SFSArray{} }

func (a *SFSArray) add(v SFSValue)              { a.items = append(a.items, v) }
func (a *SFSArray) AddInt(val int32)            { a.add(SFSValue{sfsInt, val}) }
func (a *SFSArray) AddSFSObject(val *SFSObject) { a.add(SFSValue{sfsObjectType, val}) }

// ---- Encoding ----

// EncodeObject serializes a top-level SFSObject to its self-describing wire
// form: tag(18) + i16 key count + per-key (UTF_STRING key + typed value).
// Returns an error (rather than panicking) if any key/string/collection
// along the way is too large to represent on the wire -- see int16Count and
// writeUtfString.
func EncodeObject(o *SFSObject) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(sfsObjectType)
	n, err := int16Count(len(o.keys), "keys")
	if err != nil {
		return nil, err
	}
	writeInt16(&buf, n)
	for _, k := range o.keys {
		if err := writeUtfString(&buf, k); err != nil {
			return nil, err
		}
		v := o.values[k]
		if err := writeTaggedValue(&buf, v); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func writeTaggedValue(buf *bytes.Buffer, v SFSValue) error {
	buf.WriteByte(v.Type)
	return writeValuePayload(buf, v)
}

func writeValuePayload(buf *bytes.Buffer, v SFSValue) error {
	switch v.Type {
	case sfsNull:
		// no payload
		return nil
	case sfsBool:
		if v.Val.(bool) {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		return nil
	case sfsByte:
		buf.WriteByte(v.Val.(byte))
		return nil
	case sfsShort:
		writeInt16(buf, v.Val.(int16))
		return nil
	case sfsInt:
		writeInt32(buf, v.Val.(int32))
		return nil
	case sfsLong:
		writeInt64(buf, v.Val.(int64))
		return nil
	case sfsFloat:
		binary.Write(buf, binary.BigEndian, math.Float32bits(v.Val.(float32)))
		return nil
	case sfsDouble:
		binary.Write(buf, binary.BigEndian, math.Float64bits(v.Val.(float64)))
		return nil
	case sfsUtfString:
		return writeUtfString(buf, v.Val.(string))
	case sfsText:
		// Same underlying representation as sfsUtfString (a Go string), but tagged sfsText on the
		// wire and length-prefixed with a 4-byte count instead of 2 (mirrors readValuePayload's
		// sfsText case).
		b := []byte(v.Val.(string))
		n, err := int32Count(len(b), "text bytes")
		if err != nil {
			return err
		}
		writeInt32(buf, n)
		buf.Write(b)
		return nil
	case sfsBoolArray:
		arr := v.Val.([]bool)
		n, err := int16Count(len(arr), "bool array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			if e {
				buf.WriteByte(1)
			} else {
				buf.WriteByte(0)
			}
		}
		return nil
	case sfsByteArray:
		// Unlike every other array type (which use a 2-byte count), ByteArray uses a bare 4-byte
		// int count (mirrors readValuePayload's sfsByteArray case -- see the comment there).
		b := v.Val.([]byte)
		n, err := int32Count(len(b), "byte array items")
		if err != nil {
			return err
		}
		writeInt32(buf, n)
		buf.Write(b)
		return nil
	case sfsShortArray:
		arr := v.Val.([]int16)
		n, err := int16Count(len(arr), "short array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt16(buf, e)
		}
		return nil
	case sfsIntArray:
		arr := v.Val.([]int32)
		n, err := int16Count(len(arr), "int array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt32(buf, e)
		}
		return nil
	case sfsLongArray:
		arr := v.Val.([]int64)
		n, err := int16Count(len(arr), "long array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt64(buf, e)
		}
		return nil
	case sfsFloatArray:
		arr := v.Val.([]float32)
		n, err := int16Count(len(arr), "float array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			binary.Write(buf, binary.BigEndian, math.Float32bits(e))
		}
		return nil
	case sfsDoubleArray:
		arr := v.Val.([]float64)
		n, err := int16Count(len(arr), "double array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			binary.Write(buf, binary.BigEndian, math.Float64bits(e))
		}
		return nil
	case sfsUtfStringArray:
		arr := v.Val.([]string)
		n, err := int16Count(len(arr), "utf string array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, s := range arr {
			if err := writeUtfString(buf, s); err != nil {
				return err
			}
		}
		return nil
	case sfsObjectType:
		inner := v.Val.(*SFSObject)
		n, err := int16Count(len(inner.keys), "keys")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, k := range inner.keys {
			if err := writeUtfString(buf, k); err != nil {
				return err
			}
			if err := writeTaggedValue(buf, inner.values[k]); err != nil {
				return err
			}
		}
		return nil
	case sfsArrayType:
		inner := v.Val.(*SFSArray)
		n, err := int16Count(len(inner.items), "items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, iv := range inner.items {
			if err := writeTaggedValue(buf, iv); err != nil {
				return err
			}
		}
		return nil
	default:
		// Every SFSDataType tag decode supports has an encode case above, except sfsClass (19),
		// which is unused/unimplemented by the game itself (see the const block) and so has never
		// had a decode case to mirror either. Every other case here can only be reached via
		// programmatically-constructed SFSValues (Put*/Add* helpers all set a valid Type), so an
		// unsupported tag here means a genuine programmer/decode-desync bug, not untrusted server
		// data -- unlike the two encode-time size limits below, this one stays a panic.
		panic(fmt.Sprintf("sfsobject: unsupported encode type %d", v.Type))
	}
}

func writeInt16(buf *bytes.Buffer, v int16) { binary.Write(buf, binary.BigEndian, v) }
func writeInt32(buf *bytes.Buffer, v int32) { binary.Write(buf, binary.BigEndian, v) }
func writeInt64(buf *bytes.Buffer, v int64) { binary.Write(buf, binary.BigEndian, v) }

// int16Count converts a length to int16 for a wire count field, returning an error instead of
// silently wrapping into a wrong count -- or panicking, as this used to -- if the value is ever
// too large to represent. Reachable from server-controlled data (e.g. a collection built up from
// a paginated server response), so it must not crash the process.
func int16Count(n int, what string) (int16, error) {
	if n > 32767 {
		return 0, fmt.Errorf("sfsobject: too many %s to encode (%d, max 32767)", what, n)
	}
	return int16(n), nil
}

// int32Count converts a length to int32 for a wire count field, returning an error instead of
// silently wrapping into a wrong count if the value is ever too large to represent. Reachable
// from server-controlled data, so it must not crash the process.
func int32Count(n int, what string) (int32, error) {
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("sfsobject: too many %s to encode (%d, max %d)", what, n, math.MaxInt32)
	}
	return int32(n), nil
}

// writeUtfString returns an error instead of panicking when s is too long to length-prefix with
// a 2-byte count -- reachable from server-controlled data (e.g. a batched join of server-supplied
// values), so it must not crash the process.
func writeUtfString(buf *bytes.Buffer, s string) error {
	b := []byte(s)
	if len(b) > 65535 {
		return fmt.Errorf("sfsobject: string too long to encode (%d bytes, max 65535)", len(b))
	}
	writeUint16(buf, uint16(len(b)))
	buf.Write(b)
	return nil
}
func writeUint16(buf *bytes.Buffer, v uint16) { binary.Write(buf, binary.BigEndian, v) }

// ---- Decoding ----

type sfsReader struct {
	data  []byte
	pos   int
	depth int
	nodes int
}

// maxNestDepth bounds how many levels of nested SFSArray/SFSObject readValuePayload will
// recurse into before returning a decode error instead of continuing -- real SFS2X payloads
// from this game have never needed anywhere close to this, and unbounded recursion here is a
// crash-the-process vector on a payload well under the existing frame-size cap.
const maxNestDepth = 64

// maxDecodedNodes bounds the total number of values a single decode may produce, independent of
// nesting depth or per-level fan-out -- an ordinary few-level-deep, wide-fan-out nested
// array/object can decode into an enormous number of total leaf nodes even while staying well
// within maxNestDepth and the wire-level maxFrameSize cap (a measured ~60MB wire payload
// decoding into multiple GB of heap via ordinary 3-level nesting). Chosen comfortably above
// anything the real ~313KB init payload has ever needed.
const maxDecodedNodes = 300_000

func (r *sfsReader) remaining() int { return len(r.data) - r.pos }

func (r *sfsReader) readByte() (byte, error) {
	if r.remaining() < 1 {
		return 0, fmt.Errorf("sfsobject: unexpected EOF reading byte")
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *sfsReader) readBytes(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("sfsobject: negative byte count: %d", n)
	}
	if r.remaining() < n {
		return nil, fmt.Errorf("sfsobject: unexpected EOF reading %d bytes (have %d)", n, r.remaining())
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// readArrayCount reads a 2-byte element count for a fixed-element-type array
// and rejects a negative value (a flipped top bit in a corrupted or hostile
// packet) instead of letting it flow into make() and panic the process.
func (r *sfsReader) readArrayCount() (int16, error) {
	n, err := r.readInt16()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("sfsobject: array negative size: %d", n)
	}
	return n, nil
}

func (r *sfsReader) readInt16() (int16, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return 0, err
	}
	return int16(binary.BigEndian.Uint16(b)), nil
}
func (r *sfsReader) readUint16() (uint16, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}
func (r *sfsReader) readInt32() (int32, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}
func (r *sfsReader) readInt64() (int64, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (r *sfsReader) readUtfString() (string, error) {
	n, err := r.readUint16()
	if err != nil {
		return "", err
	}
	b, err := r.readBytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *sfsReader) readTaggedValue() (SFSValue, error) {
	tag, err := r.readByte()
	if err != nil {
		return SFSValue{}, err
	}
	return r.readValuePayload(tag)
}

func (r *sfsReader) readValuePayload(tag byte) (SFSValue, error) {
	// Count every decoded value, not just containers -- leaf-node count is what actually
	// drives heap amplification for a wide-fan-out nested array/object (see maxDecodedNodes).
	r.nodes++
	if r.nodes > maxDecodedNodes {
		return SFSValue{}, fmt.Errorf("sfsobject: decoded node count exceeds %d", maxDecodedNodes)
	}
	switch tag {
	case sfsNull:
		return SFSValue{tag, nil}, nil
	case sfsBool:
		b, err := r.readByte()
		return SFSValue{tag, b != 0}, err
	case sfsByte:
		b, err := r.readByte()
		return SFSValue{tag, b}, err
	case sfsShort:
		v, err := r.readInt16()
		return SFSValue{tag, v}, err
	case sfsInt:
		v, err := r.readInt32()
		return SFSValue{tag, v}, err
	case sfsLong:
		v, err := r.readInt64()
		return SFSValue{tag, v}, err
	case sfsFloat:
		b, err := r.readBytes(4)
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, math.Float32frombits(binary.BigEndian.Uint32(b))}, nil
	case sfsDouble:
		b, err := r.readBytes(8)
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, math.Float64frombits(binary.BigEndian.Uint64(b))}, nil
	case sfsUtfString:
		s, err := r.readUtfString()
		return SFSValue{tag, s}, err
	case sfsText:
		n, err := r.readInt32()
		if err != nil {
			return SFSValue{}, err
		}
		if n < 0 {
			return SFSValue{}, fmt.Errorf("sfsobject: text negative size: %d", n)
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, string(b)}, nil
	case sfsBoolArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]bool, n)
		for i := range out {
			b, err := r.readByte()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = b != 0
		}
		return SFSValue{tag, out}, nil
	case sfsByteArray:
		// Unlike every other array type (which use a 2-byte GetTypedArraySize
		// count), ByteArray uses a bare 4-byte int count
		// (BinDecode_BYTE_ARRAY, Smartfox2xLw.decompiled.cs:7230-7238) --
		// confirmed the hard way: this 2-byte read silently misaligned
		// every subsequent field whenever a real byte-array value showed
		// up, which never happened in any packet small enough to hand-
		// inspect before the ~313KB init payload from a live capture.
		n, err := r.readInt32()
		if err != nil {
			return SFSValue{}, err
		}
		if n < 0 {
			return SFSValue{}, fmt.Errorf("sfsobject: byte array negative size: %d", n)
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, append([]byte(nil), b...)}, nil
	case sfsShortArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]int16, n)
		for i := range out {
			v, err := r.readInt16()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsIntArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]int32, n)
		for i := range out {
			v, err := r.readInt32()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsLongArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]int64, n)
		for i := range out {
			v, err := r.readInt64()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsFloatArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]float32, n)
		for i := range out {
			b, err := r.readBytes(4)
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = math.Float32frombits(binary.BigEndian.Uint32(b))
		}
		return SFSValue{tag, out}, nil
	case sfsDoubleArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]float64, n)
		for i := range out {
			b, err := r.readBytes(8)
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = math.Float64frombits(binary.BigEndian.Uint64(b))
		}
		return SFSValue{tag, out}, nil
	case sfsUtfStringArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		out := make([]string, n)
		for i := range out {
			s, err := r.readUtfString()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = s
		}
		return SFSValue{tag, out}, nil
	case sfsArrayType:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		r.depth++
		defer func() { r.depth-- }()
		if r.depth > maxNestDepth {
			return SFSValue{}, fmt.Errorf("sfsobject: nesting depth exceeds %d", maxNestDepth)
		}
		arr := NewSFSArray()
		for i := int16(0); i < n; i++ {
			v, err := r.readTaggedValue()
			if err != nil {
				return SFSValue{}, err
			}
			arr.items = append(arr.items, v)
		}
		return SFSValue{tag, arr}, nil
	case sfsObjectType:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		r.depth++
		defer func() { r.depth-- }()
		if r.depth > maxNestDepth {
			return SFSValue{}, fmt.Errorf("sfsobject: nesting depth exceeds %d", maxNestDepth)
		}
		obj := NewSFSObject()
		for i := int16(0); i < n; i++ {
			key, err := r.readUtfString()
			if err != nil {
				return SFSValue{}, err
			}
			vtag, err := r.readByte()
			if err != nil {
				return SFSValue{}, err
			}
			v, err := r.readValuePayload(vtag)
			if err != nil {
				return SFSValue{}, err
			}
			obj.put(key, v)
		}
		return SFSValue{tag, obj}, nil
	default:
		return SFSValue{}, fmt.Errorf("sfsobject: unsupported decode tag %d at pos %d", tag, r.pos-1)
	}
}

// DecodeObject parses a self-describing SFSObject blob (leading tag byte 18).
//
// Every real caller (conn.go, decode.go) hands this an exact-length frame body, so any bytes left
// over after the top-level object is fully decoded mean the encode/decode walk desynced somewhere
// -- the same class of silent misalignment the sfsByteArray count-width bug caused before it was
// caught (see the comment on that case above). Rather than risk repeating that, an unconsumed
// remainder is treated as a decode error instead of being silently accepted.
func DecodeObject(data []byte) (*SFSObject, error) {
	r := &sfsReader{data: data}
	tag, err := r.readByte()
	if err != nil {
		return nil, err
	}
	if tag != sfsObjectType {
		return nil, fmt.Errorf("sfsobject: expected top-level tag 18 (SFS_OBJECT), got %d", tag)
	}
	v, err := r.readValuePayload(tag)
	if err != nil {
		return nil, err
	}
	if rem := r.remaining(); rem > 0 {
		return nil, fmt.Errorf("sfsobject: %d trailing bytes after decoded object", rem)
	}
	return v.Val.(*SFSObject), nil
}
