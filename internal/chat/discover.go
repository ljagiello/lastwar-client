package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The discovery URLs on the global (non-China, Online) servers, in the client's order
// (ChatNetManager:Init, ChatNetManager.lua:471-486). The client starts with the URL whose type
// matches the game socket's line (RequestServerList, :596-605); this starts with the first.
var (
	linksURLs = []string{
		"https://lastwar-chat-cf.lastwarapp.net/server/links",
		"https://lastwar-chat-us-aws-ali.lastwargame.com/server/links",
		"https://lastwar-chat-us-gcp-ali.lastwargame.com/server/links",
	}
	newLineLinksURLs = []string{
		"https://lastwar-serverlist-cf.lastwarapp.net/gameservice/chat_server_links.php",
		"https://lastwar-serverlist-us-aws-ali.lastwargame.com/gameservice/chat_server_links.php",
		"https://lastwar-serverlist-us-gcp-ali.lastwargame.com/gameservice/chat_server_links.php",
	}
)

const maxDiscoveryBytes = 64 << 10

// Line is one chat server from the discovery reply.
type Line struct {
	Protocol string
	IP       string
	Port     int
}

func (l Line) url() string {
	return l.Protocol + "://" + net.JoinHostPort(l.IP, strconv.Itoa(l.Port))
}

// discoveryForm is ChatService.RequestServerList's form, fields in its order: t (server seconds),
// s = MD5(appId + uid + MD5(MD5(t[:3]) + MD5(t[len-3:]))), a, u, serverId (the self server). The
// China-only f=cn is never sent (A-CS:70626-70660).
func discoveryForm(id Identity, serverSeconds int64) string {
	t := strconv.FormatInt(serverSeconds, 10)
	s := md5Hex(AppID + id.UID + md5Hex(md5Hex(t[:min(3, len(t))])+md5Hex(t[max(0, len(t)-3):])))
	fields := [][2]string{{"t", t}, {"s", s}, {"a", AppID}, {"u", id.UID}, {"serverId", strconv.FormatInt(id.SelfServerID, 10)}}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = f[0] + "=" + url.QueryEscape(f[1])
	}
	return strings.Join(parts, "&")
}

// discover asks the discovery URLs in turn, opt.Sleep apart, and returns the first reply's lines.
// Any failure moves to the next URL, as __onRequest's retry does for an empty, undecodable or
// code != 1 reply (ChatNetManager.lua:85-123). One pass, no endless retry.
func discover(ctx context.Context, id Identity, opt Options) ([]Line, error) {
	urls := linksURLs
	if opt.NewLineList {
		urls = newLineLinksURLs
	}
	var errs []error
	for i, u := range urls {
		if i > 0 {
			opt.Sleep(3 * time.Second)
		}
		lines, err := discoverOnce(ctx, opt, u, discoveryForm(id, opt.Now().Unix()))
		if err == nil {
			return lines, nil
		}
		host := u
		if pu, perr := url.Parse(u); perr == nil {
			host = pu.Host
		}
		slog.Warn("chat: discovery failed", "host", host, "error", err)
		errs = append(errs, fmt.Errorf("%s: %v", host, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func discoverOnce(ctx context.Context, opt Options, u, form string) ([]Line, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return nil, flat("%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBytes+1))
	if err != nil {
		return nil, flat("%v", err)
	}
	if len(body) > maxDiscoveryBytes {
		return nil, fmt.Errorf("reply over %d bytes", maxDiscoveryBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var reply struct {
		Code    json.RawMessage   `json:"code"`
		Data    []json.RawMessage `json:"data"`
		DataNew []json.RawMessage `json:"data_new"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("undecodable reply (%d bytes)", len(body))
	}
	code, _ := num(reply.Code)
	data, dataNew := parseLines(reply.Data), parseLines(reply.DataNew)
	slog.Info("chat: discovery reply", "code", code, "lines", len(data), "linesNew", len(dataNew),
		"listedLines", len(reply.Data), "listedLinesNew", len(reply.DataNew), "useDataNew", opt.UseDataNew)
	if code != 1 {
		return nil, fmt.Errorf("code %d", code)
	}
	lines := data
	if opt.UseDataNew && len(dataNew) > 0 {
		lines = dataNew
	}
	if len(lines) == 0 {
		return nil, errors.New("no usable line in the reply")
	}
	return lines, nil
}

// parseLines keeps the entries with a ws/wss protocol, a host and a port in range.
func parseLines(raw []json.RawMessage) []Line {
	var out []Line
	for _, r := range raw {
		var m map[string]json.RawMessage
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		proto, _ := str(m["protocol"])
		ip, _ := str(m["ip"])
		port, ok := num(m["port"])
		proto = strings.ToLower(strings.TrimSpace(proto))
		if (proto != "ws" && proto != "wss") || ip == "" || strings.ContainsAny(ip, "/?#@ ") || !ok || port <= 0 || port > 65535 {
			continue
		}
		out = append(out, Line{Protocol: proto, IP: ip, Port: int(port)})
	}
	return out
}
