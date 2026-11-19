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

// fetchCloudConf POSTs the cloudconf request and decodes the reply's config
// with key. The reply is {code, requestId, detail}; the config is detail when
// it holds "data" (l111l11IlIlIl.java:14-17).
func fetchCloudConf(client Client, pub *rsa.PublicKey, in Install, sid string) (cloudConf, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return cloudConf{}, fmt.Errorf("shumei: cloudconf key: %w", err)
	}
	body, err := cloudConfRequest(pub, in, sid, key)
	if err != nil {
		return cloudConf{}, err
	}
	req, err := http.NewRequest(http.MethodPost, DefaultCloudConfURL, bytes.NewReader(body))
	if err != nil {
		return cloudConf{}, fmt.Errorf("shumei: build cloudconf request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Connection", "Close")
	resp, err := client.Do(req)
	if err != nil {
		return cloudConf{}, fmt.Errorf("shumei: post cloudconf: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if err != nil {
		return cloudConf{}, fmt.Errorf("shumei: read cloudconf reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return cloudConf{}, fmt.Errorf("shumei: cloudconf status %d: %s", resp.StatusCode, truncate(raw))
	}
	var reply struct {
		Code   int             `json:"code"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return cloudConf{}, fmt.Errorf("shumei: parse cloudconf reply: %w", err)
	}
	if len(reply.Detail) == 0 || reply.Detail[0] != '{' {
		return cloudConf{}, fmt.Errorf("shumei: cloudconf reply (code %d) has no config", reply.Code)
	}
	return decodeCloudConf(reply.Detail, key)
}

// decodeCloudConf decodes a cloud config as l11l111l1lll.l1111l111111Il
// (String, String) does: base64 data, AES-256-CBC with key and the fixed IV,
// zlib inflate when enc is 1, then the "ver" 1 parser (flat "sensitive.<name>"
// switches, a false one turns <name> off) or the older one (a "sensitive"
// object, a true one turns it off). The md5 is of the config text
// (l1l1l11Ill.l11l1111lIIl).
func decodeCloudConf(detail []byte, key []byte) (cloudConf, error) {
	var d struct {
		Ver  int    `json:"ver"`
		Enc  int    `json:"enc"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(detail, &d); err != nil {
		return cloudConf{}, fmt.Errorf("shumei: parse cloud config: %w", err)
	}
	if d.Data == "" {
		return cloudConf{}, errors.New("shumei: cloud config has no data")
	}
	ct, err := base64.StdEncoding.DecodeString(d.Data)
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

// sensitiveOff returns, in document order, the names a config's sensitive
// switches turn off. flat is the ver-1 form, "sensitive.<name>": bool at the
// top level, where JSONObject.optBoolean counts anything but true (or "true")
// as false; otherwise it is a "sensitive" object whose true (or "true") values
// turn their names off (l11l111l1lll.java:107-122, 230-243).
func sensitiveOff(config []byte, flat bool) ([]string, error) {
	var off []string
	err := orderedObject(config, func(key string, val json.RawMessage) {
		switch {
		case flat:
			if name, ok := strings.CutPrefix(key, "sensitive."); ok && !javaTrue(val) {
				off = append(off, name)
			}
		case key == "sensitive":
			// A "sensitive" member that isn't an object turns nothing off, as
			// the SDK's caught exception leaves the set empty.
			_ = orderedObject(val, func(name string, on json.RawMessage) {
				if javaTrue(on) {
					off = append(off, name)
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
