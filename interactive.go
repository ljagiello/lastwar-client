package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// RunInteractive keeps the connection alive (heartbeat already running)
// and reads commands from a control FIFO, one per line, of the form:
//
//	cmd.name {"key":"value","num":123,"flag":true}
//
// The JSON params are optional (bare "cmd.name" sends an empty params
// object). Every command is sent as an Extension call and any response
// sharing the same cmd (or a "push."-prefixed variant) within the next 8s
// is logged. This exists so we can experiment with many candidate
// commands against one authenticated session instead of re-running the
// full email-verification login for every test.
func RunInteractive(conn *GameConn, controlPipe string) {
	slog.Info("interactive mode: reading commands", "controlPipe", controlPipe)
	slog.Info(`format: cmd.name {"key":"value"} (params optional)`)
	slog.Info("example usage", "example", `echo 'building.camp.collect {"uuid":123}' > `+controlPipe)

	for {
		f, err := os.Open(controlPipe)
		if err != nil {
			slog.Error("open control pipe failed", "controlPipe", controlPipe, "error", err)
			os.Exit(1)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			handleInteractiveLine(conn, line)
		}
		f.Close()
		// A FIFO reader sees EOF once every writer closes; reopen and
		// keep waiting for the next command instead of exiting.
	}
}

func handleInteractiveLine(conn *GameConn, line string) {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)

	params := NewSFSObject()
	if rest != "" {
		// UseNumber: uuids routinely exceed float64's 53-bit exact-integer
		// range (2^53 ~ 16 digits; uuids here run to 19), so the default
		// map[string]any decoding (which always produces float64) silently
		// corrupts them. json.Number preserves the original decimal text.
		dec := json.NewDecoder(strings.NewReader(rest))
		dec.UseNumber()
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			slog.Error("bad JSON params", "rawParams", rest, "error", err)
			return
		}
		for k, v := range raw {
			if !putJSONValue(params, k, v) {
				slog.Error("aborting send: unsupported param value, would send incomplete params", "cmd", cmd, "key", k)
				return
			}
		}
	}

	slog.Info("sending command", "cmd", cmd, "params", params.String())
	if err := conn.SendExtension(cmd, params); err != nil {
		slog.Error("send failed -- connection appears dead, exiting interactive mode", "error", err)
		os.Exit(1)
	}

	msg, err := waitForCmd(conn, 8*time.Second, cmd, "push."+cmd)
	if err != nil {
		slog.Error("no matching response within 8s", "error", err)
		return
	}
	slog.Info("received response", "cmd", msg.Cmd, "params", msg.Params.String())
}

func putJSONValue(o *SFSObject, key string, v any) bool {
	switch val := v.(type) {
	case string:
		o.PutUtfString(key, val)
	case bool:
		o.PutBool(key, val)
	case json.Number:
		if i, err := val.Int64(); err == nil {
			o.PutLong(key, i)
		} else if f, err := val.Float64(); err == nil {
			o.PutDouble(key, f)
		} else {
			slog.Error("unparseable JSON number", "key", key, "value", val.String())
			return false
		}
	default:
		slog.Error("unsupported JSON value type", "key", key, "type", fmt.Sprintf("%T", v))
		return false
	}
	return true
}
