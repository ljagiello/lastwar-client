package shumei

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// DefaultCloudConfURL is the game's ShuMei cloud-config endpoint
// (SmSdkManager.callCreate's setConfUrl), on the same proxy as the device
// profile.
const DefaultCloudConfURL = "https://shumei-api.lastwargame.com:19091/v3/cloudconf"

// SDKVersion is the cloudconf request's sdkver (l11l11lI1lll.l111l11111I1l),
// the same "3.14.3" the device data carries in a7.
const SDKVersion = "3.14.3"

// builtinCloudConfMD5 and builtinCloudConfExcluded describe the cloud config
// the SDK carries (l11l111l1lll.l111l11111I1l, decoded with its key
// l111l11111lIl: 15,108 bytes, ver 1). It is the config in force until a
// cloudconf reply has been cached: its md5 goes out in the first request, and
// the names its "sensitive.*" keys turn off (gps and tel, in that order in the
// document) join notCollect in a85. testdata/cloudconf_builtin.json pins both.
const builtinCloudConfMD5 = "dcce7a4761131c32012fa53ccb09d77b"

var builtinCloudConfExcluded = []string{"gps", "tel"}

// notCollect is the set SmSdkManager.callCreate passes to setNotCollect, in
// the order it adds them.
var notCollect = []string{"input", "protect"}

// cloudConfIV is the fixed AES-CBC IV of the cloud config (l11l11l1lIl.java:37),
// the same one the device data uses.
var cloudConfIV = []byte("0102030405060708")

// cloudConf is a decoded cloud config: the md5 of its text, which the next
// request sends back, and the slot groups its "sensitive" switches turn off.
type cloudConf struct {
	MD5      string
	Excluded []string
}

// cloudConfRequest builds the /v3/cloudconf body (l11l111l1lll.java:56-73):
// the organization, then data {os, sdkver, md5 of the config in force, enc 1,
// bb the stored smid ("" when none: JSONObject keeps empty strings), sid,
// pri}. pri is the fresh AES-256 key the reply is encrypted with, RSA-OAEP
// (SHA-256) wrapped with the SDK's certificate and base64'd without line
// breaks (l11l11l1lIl.java:17-27). Field order is JSONObject's insertion order.
func cloudConfRequest(pub *rsa.PublicKey, in Install, sid string, key []byte) ([]byte, error) {
	pri, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, key, nil)
	if err != nil {
		return nil, fmt.Errorf("shumei: rsa-oaep pri: %w", err)
	}
	md5Hex := in.CloudConfMD5
	if md5Hex == "" {
		md5Hex = builtinCloudConfMD5
	}
	data := struct {
		OS     string `json:"os"`
		SDKVer string `json:"sdkver"`
		MD5    string `json:"md5"`
		Enc    int    `json:"enc"`
		BB     string `json:"bb"`
		SID    string `json:"sid"`
		Pri    string `json:"pri"`
	}{OSName, SDKVersion, md5Hex, 1, strings.TrimPrefix(in.BoxID, "B"), sid, base64.StdEncoding.EncodeToString(pri)}
	body := struct {
		Organization string `json:"organization"`
		Data         any    `json:"data"`
	}{Organization, data}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, fmt.Errorf("shumei: marshal cloudconf request: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// errCloudConfUndecodable marks a cloudconf reply that carried a config (a
// detail with "data") that does not decode. The SDK's success handler takes
// such a reply anyway: it sets the config in force to the failed decode's null
// and saves the reply (l11l111l1lll.java:30-39), so from then on the cached
// config fails to decode too and the built-in one is in force
// (l111l11111lIl(), :296-309). Register clears the cache for it.
var errCloudConfUndecodable = errors.New("shumei: the cloud config in the cloudconf reply does not decode")

// cloudConfReply is what a cloudconf reply said besides its config: code
// (optInt "code", -1 when absent) and requestId, both only logged.
type cloudConfReply struct {
	Code      string
	RequestID string
}

// fetchCloudConf POSTs the cloudconf request and decodes the reply's config
// with key. The reply is {code, requestId, detail}; the config is detail when
// it is an object holding "data" (l111l11IlIlIl.java:14-17). Anything else,
// like a failed POST, is the SDK's error path, whose handler is empty
// (l11l111l1lll.java:79-86): the cached config stays. A detail with "data"
// that does not decode is errCloudConfUndecodable.
func fetchCloudConf(client Client, pub *rsa.PublicKey, in Install, sid string) (cloudConf, cloudConfReply, error) {
	var meta cloudConfReply
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: cloudconf key: %w", err)
	}
	body, err := cloudConfRequest(pub, in, sid, key)
	if err != nil {
		return cloudConf{}, meta, err
	}
	req, err := http.NewRequest(http.MethodPost, DefaultCloudConfURL, bytes.NewReader(body))
	if err != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: build cloudconf request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Connection", "Close")
	resp, err := client.Do(req)
	if err != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: post cloudconf: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if err != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: read cloudconf reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return cloudConf{}, meta, fmt.Errorf("shumei: cloudconf status %d: %s", resp.StatusCode, truncate(raw))
	}
	var reply struct {
		Code      json.RawMessage `json:"code"`
		RequestID json.RawMessage `json:"requestId"`
		Detail    json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: parse cloudconf reply: %w", err)
	}
	meta.Code = "-1"
	if len(reply.Code) > 0 {
		meta.Code = string(reply.Code)
	}
	if json.Unmarshal(reply.RequestID, &meta.RequestID) != nil {
		meta.RequestID = string(reply.RequestID)
	}
	var detail map[string]json.RawMessage
	if len(reply.Detail) == 0 || reply.Detail[0] != '{' || json.Unmarshal(reply.Detail, &detail) != nil {
		return cloudConf{}, meta, fmt.Errorf("shumei: cloudconf reply (code %s) has no config", meta.Code)
	}
	if _, ok := detail["data"]; !ok {
		return cloudConf{}, meta, fmt.Errorf("shumei: cloudconf reply (code %s) has no config data", meta.Code)
	}
	conf, err := decodeCloudConf(reply.Detail, key)
	if err != nil {
		return cloudConf{}, meta, fmt.Errorf("%w: %w", errCloudConfUndecodable, err)
	}
	return conf, meta, nil
}

// decodeCloudConf decodes a cloud config as l11l111l1lll.l1111l111111Il
// (String, String) does: base64 data, AES-256-CBC with key and the fixed IV,
// zlib inflate when enc is 1, then the "ver" 1 parser (flat "sensitive.<name>"
// switches, a false one turns <name> off) or the older one (a "sensitive"
// object, a true one turns it off). The md5 is of the config text
// (l1l1l11Ill.l11l1111lIIl).
func decodeCloudConf(detail []byte, key []byte) (cloudConf, error) {
	var d struct {
		Ver    int             `json:"ver"`
		Enc    int             `json:"enc"`
		Data   string          `json:"data"`
		Length json.RawMessage `json:"length"`
	}
	if err := json.Unmarshal(detail, &d); err != nil {
		return cloudConf{}, fmt.Errorf("shumei: parse cloud config: %w", err)
	}
	// The SDK reads getInt("length") first, so a config without a numeric
	// length is dropped (l11l111l1lll.java:249); the value itself is unused.
	if !javaInt(d.Length) {
		return cloudConf{}, errors.New("shumei: cloud config has no numeric length")
	}
	if d.Data == "" {
		return cloudConf{}, errors.New("shumei: cloud config has no data")
	}
	ct, err := javaBase64Decode(d.Data)
	if err != nil {
		return cloudConf{}, fmt.Errorf("shumei: cloud config data: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return cloudConf{}, fmt.Errorf("shumei: cloud config key: %w", err)
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return cloudConf{}, fmt.Errorf("shumei: cloud config data is %d bytes, not whole AES blocks", len(ct))
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, cloudConfIV).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(pt[len(pt)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return cloudConf{}, errors.New("shumei: cloud config data has no PKCS#7 padding (wrong key?)")
	}
	pt = pt[:len(pt)-pad]
	if d.Enc == 1 {
		zr, err := zlib.NewReader(bytes.NewReader(pt))
		if err != nil {
			return cloudConf{}, fmt.Errorf("shumei: inflate cloud config: %w", err)
		}
		pt, err = io.ReadAll(io.LimitReader(zr, 4*MaxResponseSize))
		if err != nil {
			return cloudConf{}, fmt.Errorf("shumei: inflate cloud config: %w", err)
		}
	}
	excluded, err := sensitiveOff(pt, d.Ver == 1)
	if err != nil {
		return cloudConf{}, err
	}
	sum := md5.Sum(pt)
	return cloudConf{MD5: hex.EncodeToString(sum[:]), Excluded: excluded}, nil
}

// sensitiveOff returns, in document order and once each (the SDK collects
// them in a HashSet), the names a config's sensitive switches turn off. flat
// is the ver-1 form, "sensitive.<name>": bool at the top level, where
// JSONObject.optBoolean counts anything but true (or "true") as false
// (l11l111l1lll.java:95-111, called at :156); the name is sensitiveName's.
// Otherwise it is a "sensitive" object whose true (or "true") values turn
// their names off (:227-240, called at :379).
func sensitiveOff(config []byte, flat bool) ([]string, error) {
	var off []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			off = append(off, name)
		}
	}
	err := orderedObject(config, func(key string, val json.RawMessage) {
		switch {
		case flat:
			if name, ok := sensitiveName(key); ok && !javaTrue(val) {
				add(name)
			}
		case key == "sensitive":
			// A "sensitive" member that isn't an object turns nothing off, as
			// the SDK's caught exception leaves the set empty.
			_ = orderedObject(val, func(name string, on json.RawMessage) {
				if javaTrue(on) {
					add(name)
				}
			})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("shumei: parse cloud config text: %w", err)
	}
	return off, nil
}

// orderedObject calls f for each member of the JSON object in obj, in
// document order (Android's JSONObject keeps insertion order).
func orderedObject(obj []byte, f func(key string, val json.RawMessage)) error {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("not a JSON object (%v)", err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return err
		}
		f(key, val)
	}
	return nil
}

// sensitiveName is the name a flat "sensitive.*" key switches:
// key.split("\\.")[1] (l11l111l1lll.java:101-104). Java's split drops
// trailing empty strings, so "sensitive.a.b" and "sensitive.a." name "a",
// "sensitive..b" names "", and "sensitive." has no index 1: the SDK's caught
// exception skips the key, so ok is false.
func sensitiveName(key string) (name string, ok bool) {
	if !strings.HasPrefix(key, "sensitive.") {
		return "", false
	}
	parts := strings.Split(key, ".")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return "", false
	}
	return parts[1], true
}

// javaInt reports whether JSONObject.getInt reads v as a number: a JSON
// number, or a string that parses as one (org.json's coercion).
func javaInt(v json.RawMessage) bool {
	var f float64
	if json.Unmarshal(v, &f) == nil {
		return true
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return false
	}
	_, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil
}

// javaBase64Decode decodes s as the SDK's Base64.decode(bytes, DEFAULT)
// (l1l1l11Ill.java:649-655): line breaks are skipped, as in the
// DEFAULT-flag output Android's encoder wraps at 76 characters (Go's decoder
// skips them too; they are removed first so the length test below sees the
// real length), and the trailing "=" padding is optional, though when
// present it must be complete.
func javaBase64Decode(s string) ([]byte, error) {
	s = strings.NewReplacer("\r", "", "\n", "").Replace(s)
	if len(s)%4 == 0 {
		return base64.StdEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// javaTrue reports whether JSONObject.optBoolean/getBoolean reads v as true:
// the boolean true or the string "true" in any case.
func javaTrue(v json.RawMessage) bool {
	var b bool
	if json.Unmarshal(v, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(v, &s) == nil && strings.EqualFold(s, "true")
}

// exclusionSet returns a85, the collector's exclusion set
// (l111l1111llIl.java:224-233): a new HashSet that gets the cloud config's
// switched-off names (in their own HashSet's order) and then notCollect (in
// its HashSet's order), listed in Java's HashSet iteration order. cloud is in
// document order.
func exclusionSet(cloud []string) []string {
	merged := append(javaHashSetOrder(cloud), javaHashSetOrder(notCollect)...)
	return javaHashSetOrder(merged)
}

// slotGates are the collector's gates (l111l1111llIl.java) that turn a slot
// off under a group name, or turn several slots off together. Every gate also
// checks each slot's own aNN key, which gateSlots does without a table. Only
// the gates of slots deviceData fills are listed. The other names a config or
// notCollect may hold (gps, tel, protect among them) gate none of those slots.
var slotGates = []struct{ names, slots []string }{
	{[]string{"oaid"}, []string{"a103"}},                                                 // :64
	{[]string{"drmId"}, []string{"a84"}},                                                 // :69
	{[]string{"xpApp"}, []string{"a102"}},                                                // :87
	{[]string{"hookJava"}, []string{"a107"}},                                             // :90
	{[]string{"bssid"}, []string{"a19"}},                                                 // :93
	{[]string{"ssid"}, []string{"a30"}},                                                  // :96
	{[]string{"wifiip"}, []string{"a31"}},                                                // :99
	{[]string{"network"}, []string{"a44"}},                                               // :102
	{[]string{"battery"}, []string{"a72"}},                                               // :106
	{[]string{"oaidCheck", "a103"}, []string{"a162"}},                                    // :136
	{[]string{"operator", "a45", "a131", "a132"}, []string{"a45", "a131", "a132"}},       // :300-315
	{[]string{"simCountryISO"}, []string{"a131"}},                                        // :306
	{[]string{"networkCountryIso"}, []string{"a132"}},                                    // :311
	{[]string{"adid"}, []string{"a24"}},                                                  // :322
	{[]string{"cpuinfo", "a33", "a143", "a34"}, []string{"a33", "a143", "a34"}},          // :337-342
	{[]string{"sensor"}, []string{"a47"}},                                                // :385
	{[]string{"a56", "a57"}, []string{"a56", "a57"}},                                     // :395-398
	{[]string{"a69", "a70", "a71"}, []string{"a69", "a70", "a71"}},                       // :411-415
	{[]string{"locationCls"}, []string{"a116"}},                                          // :443
	{[]string{"launcherInfo"}, []string{"a154"}},                                         // :513
	{[]string{"virtualUid"}, []string{"a60"}},                                            // :775
	{[]string{"sysEnv"}, []string{"a115"}},                                               // :798
	{[]string{"screenRecord", "a141", "a156", "a157"}, []string{"a141", "a156", "a157"}}, // :837-844
}

// primitiveSlots are the slots the SDK's data holder (l11l1111lIIl.java)
// declares as a primitive int, long or boolean, with that zero. The serializer
// drops only null and empty strings, collections and maps
// (l1l1l11Ill.java:231-254, 633-647), so a gated primitive still goes out, as
// its zero.
var primitiveSlots = map[string]any{
	"a113": 0, "a117": false, "a121": 0, "a127": 0, "a130": 0, "a133": 0,
	"a135": 0, "a139": 0, "a141": 0, "a142": 0, "a155": 0, "a157": 0,
	"a160": 0, "a161": 0, "a164": 0, "a165": 0, "a167": 0, "a168": 0,
	"a170": 0, "a171": 0, "a173": 0, "a174": 0,
}

// gateSlots applies the exclusion set (a85) to the filled slots m the way the
// collector does: it checks the same set it reports (l111l1111llIl.java:
// 224-253) before filling each slot, by the slot's own key or by a group name
// (slotGates). A gated slot is dropped, or sent as its zero when the SDK holds
// it in a primitive.
func gateSlots(m map[string]any, set []string) {
	off := make(map[string]bool, len(set))
	for _, name := range set {
		off[name] = true
	}
	var gated []string
	for slot := range m {
		if off[slot] {
			gated = append(gated, slot)
		}
	}
	for _, g := range slotGates {
		if slices.ContainsFunc(g.names, func(name string) bool { return off[name] }) {
			gated = append(gated, g.slots...)
		}
	}
	for _, slot := range gated {
		if _, filled := m[slot]; !filled {
			continue
		}
		if zero, ok := primitiveSlots[slot]; ok {
			m[slot] = zero
		} else {
			delete(m, slot)
		}
	}
}

// javaHashSetOrder returns the distinct strings of insertion as a new
// java.util.HashSet<String> filled in that order iterates them: by bucket
// ((h ^ h>>>16) & (capacity-1), h = String.hashCode()), then by insertion
// within a bucket. The capacity starts at 16 and doubles past a 0.75 load,
// and a resize keeps each bucket's insertion order.
func javaHashSetOrder(insertion []string) []string {
	var uniq []string
	seen := map[string]bool{}
	for _, s := range insertion {
		if !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	capacity := uint32(16)
	for uint32(len(uniq)) > capacity*3/4 {
		capacity *= 2
	}
	buckets := make([][]string, capacity)
	for _, s := range uniq {
		h := javaStringHashCode(s)
		i := (h ^ h>>16) & (capacity - 1)
		buckets[i] = append(buckets[i], s)
	}
	out := make([]string, 0, len(uniq))
	for _, b := range buckets {
		out = append(out, b...)
	}
	return out
}

// javaStringHashCode is String.hashCode() over the UTF-16 code units, as an
// unsigned 32-bit value.
func javaStringHashCode(s string) uint32 {
	var h uint32
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			h = 31*h + uint32(0xd800+(r>>10))
			h = 31*h + uint32(0xdc00+(r&0x3ff))
			continue
		}
		h = 31*h + uint32(r)
	}
	return h
}
