package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"

	"lastwar-client/internal/app"
	"lastwar-client/internal/pcap"
	"lastwar-client/internal/sfs"
)

// runSessionOut implements -session-out: it finds the real app's Login request (c=0, a=1) in a
// capture, checks that the server accepted it, and writes the reconnect credentials it carried
// (accessToken, shumeiBoxId, deviceId, gameUid, zone, game server ip/port, iOS-vs-Android) as the
// session config JSON that lastwar-client auto-loads. This is the whole recovery procedure for the
// ec=28/E011 "token rotated" failure (README, "Recognizing an expired token"): capture the real app
// logging in, run this, copy the file into place.
//
// The credentials are never printed -- stdout only names which fields were found and their lengths,
// so the output is safe to paste into an issue or a terminal log.
//
// stream < 0 searches every plain (non-TLS) conversation. The real client races the zone port on
// several gateway IPs with a=29 probes before logging in on the fastest, so a capture normally holds
// exactly one accepted Login; if it holds more (e.g. the app was relaunched mid-capture) the
// conversations carry no timestamps to tell which is freshest, so this refuses and asks for -stream.
func runSessionOut(in string, stream int, clientStr, path string) error {
	if in == "" {
		return errors.New("-in is required")
	}
	if path == "" {
		return errors.New("-session-out needs a destination path")
	}
	var explicit netip.Addr
	if clientStr != "" {
		var err error
		if explicit, err = netip.ParseAddr(clientStr); err != nil {
			return fmt.Errorf("bad -client %q: %w", clientStr, err)
		}
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	pkts, err := pcap.Parse(data)
	if err != nil {
		return err
	}
	convs := pcap.Conversations(pkts)
	if len(convs) == 0 {
		return fmt.Errorf("no TCP conversations with payload found in %s", in)
	}

	candidates := []int{stream}
	if stream < 0 {
		candidates = candidates[:0]
		for i, c := range convs {
			if !c.TLS {
				candidates = append(candidates, i)
			}
		}
	} else if stream >= len(convs) {
		return fmt.Errorf("choose a -stream between 0 and %d (run with -list to see them)", len(convs)-1)
	}

	var found []int
	var cfg *app.SessionConfig
	var rejected []string
	var gateways []pcap.Endpoint // other gateways that answered the a=29 probe
	for _, i := range candidates {
		conv := convs[i]
		client, err := conv.Client(explicit)
		if err != nil {
			if stream >= 0 {
				return err
			}
			continue
		}
		c2s, s2c := conv.Reassemble(client)
		got, reason := acceptedLogin(c2s, s2c)
		if got == nil {
			if reason != "" {
				rejected = append(rejected, fmt.Sprintf("stream %d: %s", i, reason))
			} else if answeredProbe(c2s, s2c) {
				gateways = append(gateways, other(conv, client))
			}
			continue
		}
		server := other(conv, client)
		got.IP = server.Addr.Unmap().String()
		got.Port = int(server.Port)
		found = append(found, i)
		cfg = got
	}

	switch {
	case len(found) == 0:
		msg := "no server-accepted Login (c=0, a=1) found in " + in
		if len(rejected) > 0 {
			msg += " (" + strings.Join(rejected, "; ") + ")"
		}
		return errors.New(msg + " -- capture the real app's cold start, from launch until its main screen loads")
	case len(found) > 1:
		return fmt.Errorf("streams %v each hold an accepted Login; pick the freshest with -stream", found)
	}
	// The gateways that lost the real client's latency race reach the same zone, so they become
	// the client's dial fallbacks, after the one the accepted Login went through.
	for _, g := range gateways {
		if host := g.Addr.Unmap().String(); int(g.Port) == cfg.Port && !slices.Contains(strings.Split(cfg.IP, "|"), host) {
			cfg.IP += "|" + host
		}
	}

	if err := writeSessionConfig(path, cfg); err != nil {
		return err
	}
	fmt.Printf("stream %d: wrote session config to %s (mode 0600)\n", found[0], path)
	fmt.Printf("  ip=%s port=%d zone=%s iosMode=%t appVersion=%s versionCode=%s\n",
		cfg.IP, cfg.Port, cfg.Zone, cfg.IOSMode, cfg.AppVersion, cfg.VersionCode)
	fmt.Printf("  gameUid (%d chars), deviceId (%d chars), shumeiBoxId (%d chars), accessToken (%d chars) -- values not shown\n",
		len(cfg.GameUid), len(cfg.DeviceID), len(cfg.ShumeiBoxId), len(cfg.AccessToken))
	return nil
}

// acceptedLogin decodes one reassembled conversation and returns the session config carried by its
// last Login request, provided the server's Login reply carries no error code. It returns nil plus a
// reason when the stream holds a Login that can't be used, and nil plus "" when it holds no Login at
// all (an a=29 gateway probe, an unrelated socket). IP/Port are left for the caller to fill in.
func acceptedLogin(c2s, s2c []byte) (*app.SessionConfig, string) {
	var req *sfs.SFSObject
	for _, obj := range decodeAll(c2s) {
		if isLogin(obj) {
			req = sfsObject(obj, "p")
		}
	}
	if req == nil {
		return nil, ""
	}
	var resp *sfs.SFSObject
	for _, obj := range decodeAll(s2c) {
		if isLogin(obj) {
			resp = sfsObject(obj, "p")
		}
	}
	if resp == nil {
		return nil, "Login sent but no Login reply captured"
	}
	if resp.Has("ec") {
		// ec/ep are SFS2X's own error fields (docs/live-validation.mdx); not sensitive.
		return nil, fmt.Sprintf("server rejected the Login (ec=%d)", resp.GetLong("ec"))
	}

	params := sfsObject(req, "p")
	cfg := &app.SessionConfig{
		Zone:        req.GetString("zn"),
		GameUid:     params.GetString("gameUid"),
		DeviceID:    params.GetString("deviceId"),
		ShumeiBoxId: params.GetString("shumeiBoxId"),
		AccessToken: params.GetString("at"),
		IOSMode:     params.GetString("platform") == "0",
		AppVersion:  params.GetString("appVersion"),
		VersionCode: params.GetString("versionCode"),
	}
	if cfg.GameUid == "" {
		cfg.GameUid = req.GetString("un")
	}
	// shumeiBoxId is copied when present but not required: a reconnect with it empty was accepted
	// live (2026-10-04).
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"zn", cfg.Zone}, {"gameUid", cfg.GameUid}, {"deviceId", cfg.DeviceID}, {"at", cfg.AccessToken},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, "accepted Login is missing " + strings.Join(missing, ", ")
	}
	return cfg, ""
}

func isLogin(obj *sfs.SFSObject) bool {
	return obj.Has("c") && obj.GetLong("c") == 0 && obj.GetLong("a") == 1
}

// answeredProbe reports whether a conversation is one of the real client's pre-login gateway
// probes (a c=0, a=29 ping) that the server answered with a pong.
func answeredProbe(c2s, s2c []byte) bool {
	isPing := func(obj *sfs.SFSObject) bool {
		return obj.Has("c") && obj.GetLong("c") == 0 && obj.GetLong("a") == 29
	}
	return slices.ContainsFunc(decodeAll(c2s), isPing) && slices.ContainsFunc(decodeAll(s2c), isPing)
}

func sfsObject(obj *sfs.SFSObject, key string) *sfs.SFSObject {
	v, ok := obj.Get(key)
	if !ok {
		return nil
	}
	o, _ := v.Val.(*sfs.SFSObject)
	return o
}

// decodeAll frames and decodes every packet in one reassembled direction, stopping quietly at the
// first framing error (a capture that starts or ends mid-packet).
func decodeAll(data []byte) []*sfs.SFSObject {
	r := bytes.NewReader(data)
	var out []*sfs.SFSObject
	for {
		body, err := sfs.ReadPacket(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "pcap: stopped decoding at offset %d: %v\n", len(data)-r.Len(), err)
			}
			return out
		}
		if obj, err := sfs.DecodeObject(body); err == nil {
			out = append(out, obj)
		}
	}
}

// writeSessionConfig writes cfg as indented JSON with 0600 permissions, tightening an existing
// file's mode too since it now holds a live access token.
func writeSessionConfig(path string, cfg *app.SessionConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
