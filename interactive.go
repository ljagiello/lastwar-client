package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())
		conn.Close()
		os.Exit(0)
	}()

	for {
		fi, statErr := os.Stat(controlPipe)
		if statErr != nil {
			slog.Error("stat control pipe failed", "controlPipe", controlPipe, "error", statErr)
			os.Exit(1)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			slog.Error("controlPipe exists but is not a FIFO -- did you forget mkfifo?", "controlPipe", controlPipe)
			os.Exit(1)
		}
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
		if err := scanner.Err(); err != nil {
			slog.Error("control pipe scan error", "controlPipe", controlPipe, "error", err)
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
			// Not "rawParams", rest -- an operator can send literally any command over the
			// control FIFO, including an account/login-family one, and a JSON typo (missing
			// quote, trailing comma) could leave a credential value sitting unparsed in rest.
			// Go's encoding/json parse error names the problem (offset, token) without needing
			// the raw text echoed back; see the "sending command" comment below for the same
			// reasoning applied to params.
			slog.Error("bad JSON params", "cmd", cmd, "error", err)
			return
		}
		for k, v := range raw {
			if !putJSONValue(params, k, v) {
				slog.Error("aborting send: unsupported param value, would send incomplete params", "cmd", cmd, "key", k)
				return
			}
		}
	}

	// Not params.String() -- an operator can send literally any command over the control FIFO,
	// including an account/login-family one, and its response may carry a live
	// loginKey/accessToken/airKey/shumeiBoxId in cleartext. StringRedacted masks those fields
	// (see sfsobject.go) instead of dumping everything raw.
	slog.Info("sending command", "cmd", cmd, "params", params.StringRedacted())
	if err := conn.SendExtension(cmd, params); err != nil {
		slog.Error("send failed -- connection appears dead, exiting interactive mode", "error", err)
		os.Exit(1)
	}

	msg, err := waitForCmd(conn, 8*time.Second, cmd, "push."+cmd)
	if err != nil {
		slog.Error("no matching response within 8s", "error", err)
		return
	}
	// Not msg.Params.String() -- same reasoning as the "sending command" log above.
	slog.Info("received response", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
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
