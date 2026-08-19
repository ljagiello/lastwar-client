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
func (o *SFSObject) PutFloat(key string, val float32)  { o.put(key, SFSValue{sfsFloat, val}) }
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
func (o *SFSObject) Keys() []string { return o.keys }

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

// SFSArray is a sequential list of tagged values.
type SFSArray struct {
	items []SFSValue
}

func NewSFSArray() *SFSArray { return &SFSArray{} }

func (a *SFSArray) add(v SFSValue)              { a.items = append(a.items, v) }
func (a *SFSArray) AddUtfString(val string)     { a.add(SFSValue{sfsUtfString, val}) }
func (a *SFSArray) AddInt(val int32)            { a.add(SFSValue{sfsInt, val}) }
func (a *SFSArray) AddLong(val int64)           { a.add(SFSValue{sfsLong, val}) }
func (a *SFSArray) AddBool(val bool)            { a.add(SFSValue{sfsBool, val}) }
func (a *SFSArray) AddSFSObject(val *SFSObject) { a.add(SFSValue{sfsObjectType, val}) }

// ---- Encoding ----

// EncodeObject serializes a top-level SFSObject to its self-describing wire
// form: tag(18) + i16 key count + per-key (UTF_STRING key + typed value).
func EncodeObject(o *SFSObject) []byte {
	var buf bytes.Buffer
	buf.WriteByte(sfsObjectType)
	writeInt16(&buf, int16(len(o.keys)))
	for _, k := range o.keys {
		writeUtfString(&buf, k)
		v := o.values[k]
		writeTaggedValue(&buf, v)
	}
	return buf.Bytes()
}

func EncodeArray(a *SFSArray) []byte {
	var buf bytes.Buffer
	buf.WriteByte(sfsArrayType)
	writeInt16(&buf, int16(len(a.items)))
	for _, v := range a.items {
		writeTaggedValue(&buf, v)
	}
	return buf.Bytes()
}

func writeTaggedValue(buf *bytes.Buffer, v SFSValue) {
	buf.WriteByte(v.Type)
	writeValuePayload(buf, v)
}

func writeValuePayload(buf *bytes.Buffer, v SFSValue) {
	switch v.Type {
	case sfsNull:
		// no payload
	case sfsBool:
		if v.Val.(bool) {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	case sfsByte:
		buf.WriteByte(v.Val.(byte))
	case sfsShort:
		writeInt16(buf, v.Val.(int16))
	case sfsInt:
		writeInt32(buf, v.Val.(int32))
	case sfsLong:
		writeInt64(buf, v.Val.(int64))
	case sfsFloat:
		binary.Write(buf, binary.BigEndian, math.Float32bits(v.Val.(float32)))
	case sfsDouble:
		binary.Write(buf, binary.BigEndian, math.Float64bits(v.Val.(float64)))
	case sfsUtfString:
		writeUtfString(buf, v.Val.(string))
	case sfsObjectType:
		inner := v.Val.(*SFSObject)
		writeInt16(buf, int16(len(inner.keys)))
		for _, k := range inner.keys {
			writeUtfString(buf, k)
			writeTaggedValue(buf, inner.values[k])
		}
	case sfsArrayType:
		inner := v.Val.(*SFSArray)
		writeInt16(buf, int16(len(inner.items)))
		for _, iv := range inner.items {
			writeTaggedValue(buf, iv)
		}
	default:
		panic(fmt.Sprintf("sfsobject: unsupported encode type %d", v.Type))
	}
}

func writeInt16(buf *bytes.Buffer, v int16) { binary.Write(buf, binary.BigEndian, v) }
func writeInt32(buf *bytes.Buffer, v int32) { binary.Write(buf, binary.BigEndian, v) }
func writeInt64(buf *bytes.Buffer, v int64) { binary.Write(buf, binary.BigEndian, v) }

func writeUtfString(buf *bytes.Buffer, s string) {
	b := []byte(s)
	writeUint16(buf, uint16(len(b)))
	buf.Write(b)
}
func writeUint16(buf *bytes.Buffer, v uint16) { binary.Write(buf, binary.BigEndian, v) }

// ---- Decoding ----

type sfsReader struct {
	data  []byte
	pos   int
	depth int
}

// maxNestDepth bounds how many levels of nested SFSArray/SFSObject readValuePayload will
// recurse into before returning a decode error instead of continuing -- real SFS2X payloads
// from this game have never needed anywhere close to this, and unbounded recursion here is a
// crash-the-process vector on a payload well under the existing frame-size cap.
const maxNestDepth = 64

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
	return v.Val.(*SFSObject), nil
}

// ToGo recursively converts an SFSValue tree into plain Go values
// (map[string]interface{} / []interface{}) for easy printing/inspection.
func (v SFSValue) ToGo() interface{} {
	switch val := v.Val.(type) {
	case *SFSObject:
		m := make(map[string]interface{}, len(val.keys))
		for _, k := range val.keys {
			m[k] = val.values[k].ToGo()
		}
		return m
	case *SFSArray:
		arr := make([]interface{}, len(val.items))
		for i, iv := range val.items {
			arr[i] = iv.ToGo()
		}
		return arr
	default:
		return val
	}
}
