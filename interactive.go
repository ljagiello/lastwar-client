package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// controlPipeRetries/controlPipeRetryDelay bound how many times RunInteractive's per-iteration
// os.Stat/os.Open calls on the control FIFO (see statControlPipeWithRetry/openControlPipeWithRetry
// below) retry a failure before giving up. Before this fix, ANY failure from either call -- even a
// transient, local hiccup like a brief permission error or the pipe being momentarily
// replaced/recreated by another process -- was immediately fatal (os.Exit(1)), discarding the
// whole authenticated session, including its already-alive game connection, over what is often
// self-correcting within milliseconds. This isn't just a startup concern: a FIFO reader sees EOF
// once every writer closes, and RunInteractive's loop reopens the pipe to wait for the next
// command, so a transient failure at reopen time is exactly as disruptive as one at startup. This
// is the same bug class shouldAbortBeforeInteractive (main.go) exists to prevent elsewhere -- an
// -interactive session shouldn't be discarded over a non-fatal, recoverable issue -- brought here.
//
// Kept intentionally small/bounded: this doesn't need to retry forever, just avoid killing a live
// session over one transient hiccup. A failure that persists past this budget is still treated as
// genuinely fatal, exactly as before this fix.
const (
	controlPipeRetries    = 5
	controlPipeRetryDelay = 50 * time.Millisecond
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
//
// Only flat scalar param values are supported: strings, bools, and
// numbers. Nested JSON objects/arrays (e.g. {"heroes":[1,2,3]}) are
// rejected by putJSONValue and abort the send -- even though the
// underlying protocol does use array-shaped params for some real
// commands (see docs/military-battle.mdx's PutSFSArray usage). There is
// no workaround via this control FIFO today; such commands cannot be
// exercised through -interactive.
func RunInteractive(conn *GameConn, controlPipe string) {
	slog.Info("interactive mode: reading commands", "controlPipe", controlPipe)
	slog.Info(`format: cmd.name {"key":"value"} (params optional)`)
	slog.Info("example usage", "example", `echo 'building.production.collect {"uuid":123}' > `+controlPipe)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())
		conn.Close()
		os.Exit(0)
	}()

	for {
		fi, statErr := statControlPipeWithRetry(controlPipe)
		if statErr != nil {
			slog.Error("stat control pipe failed", "controlPipe", controlPipe, "error", statErr, "retries", controlPipeRetries)
			os.Exit(1)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			// Not retried, unlike the stat/open failures above/below: a path that exists but isn't
			// a FIFO is a permanent misconfiguration (-interactive pointed at a plain file, or
			// mkfifo was simply never run), not a transient condition that resolves itself if we
			// just wait and look again.
			slog.Error("controlPipe exists but is not a FIFO -- did you forget mkfifo?", "controlPipe", controlPipe)
			os.Exit(1)
		}
		f, err := openControlPipeWithRetry(controlPipe)
		if err != nil {
			slog.Error("open control pipe failed", "controlPipe", controlPipe, "error", err, "retries", controlPipeRetries)
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

// statControlPipeWithRetry stats controlPipe, retrying up to controlPipeRetries times (with a
// controlPipeRetryDelay pause between attempts) before giving up -- see controlPipeRetries' own
// doc comment for why a single os.Stat failure here must not be immediately fatal the way it used
// to be. Returns the last error seen if every attempt fails.
func statControlPipeWithRetry(controlPipe string) (os.FileInfo, error) {
	var fi os.FileInfo
	var err error
	for attempt := 0; attempt <= controlPipeRetries; attempt++ {
		fi, err = os.Stat(controlPipe)
		if err == nil {
			return fi, nil
		}
		if attempt < controlPipeRetries {
			time.Sleep(controlPipeRetryDelay)
		}
	}
	return nil, err
}

// openControlPipeWithRetry opens controlPipe for reading, retrying up to controlPipeRetries times
// (with a controlPipeRetryDelay pause between attempts) before giving up -- symmetric to
// statControlPipeWithRetry above, and for the identical reason (see controlPipeRetries' own doc
// comment). Returns the last error seen if every attempt fails.
func openControlPipeWithRetry(controlPipe string) (*os.File, error) {
	var f *os.File
	var err error
	for attempt := 0; attempt <= controlPipeRetries; attempt++ {
		f, err = os.Open(controlPipe)
		if err == nil {
			return f, nil
		}
		if attempt < controlPipeRetries {
			time.Sleep(controlPipeRetryDelay)
		}
	}
	return nil, err
}

func handleInteractiveLine(conn *GameConn, line string) {
	cmd, rest, found := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)

	if !found && strings.Contains(cmd, "{") {
		// No space was found at all, and what strings.Cut left us in cmd contains a '{':
		// almost certainly a JSON params blob glued onto the command name with no
		// separating space (e.g. "cmd.name{\"uuid\":123}", or even bare "{\"uuid\":123}"
		// with no command prefix), not a legitimate bare command -- this tool's command
		// names are plain dot-separated identifiers (see the flat-scalar-only command
		// format documented above) and never contain '{' themselves. Left unchecked, cmd
		// here is the *entire* raw line and would be sent as a literal SFS2X command name
		// with empty params, with no error until the far less obvious "no matching
		// response within 8s" several seconds later -- the "if rest != \"\"" JSON-decode
		// block below never even runs, since rest is always "" in this case. Don't echo
		// cmd/line itself in the log -- same "do not echo the raw text" reasoning as the
		// JSON-decode-error branch below: the glued-on JSON could contain a credential
		// value the operator meant to pass as params. The part before the first '{' is
		// safe to log, though (it can't contain JSON content), and is a useful hint at
		// what the operator likely meant to send.
		likelyCmd, _, _ := strings.Cut(cmd, "{")
		slog.Error("bad JSON params", "cmd", likelyCmd, "error", fmt.Errorf("missing space between command name and JSON params"))
		return
	}

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
		if dec.More() {
			// Decode only consumes the first well-formed JSON value on the line -- it
			// doesn't error just because there are leftover bytes after it (e.g. a stray
			// trailing token, or a second concatenated JSON value). Left unchecked, that
			// trailing text is silently discarded and the command sends with an
			// incomplete/misleading view of what the operator typed. Same "do not echo
			// the raw text" reasoning as the Decode-error branch above applies here too,
			// since the trailing text could itself be an accidentally-appended
			// credential fragment.
			slog.Error("bad JSON params", "cmd", cmd, "error", fmt.Errorf("trailing data after JSON value"))
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

	msg, err := waitForCmd(conn, defaultCmdTimeout, cmd, "push."+cmd)
	if err != nil {
		// Same net.Error/Timeout() distinction round 21 applied at 6+ other call sites
		// (buildings.go, mail.go, visitors.go, alliance.go) -- and that's already honored two
		// lines up by SendExtension's own unconditional-fatal treatment on send failure.
		// waitForCmd's ordinary "no matching response within 8s" outcome is ITSELF a net.Error
		// with Timeout()==true (conn_wait_test.go's TestWaitForTimeout): the operator's command
		// simply had no reply within the window, which is routine and expected -- not evidence
		// the connection died -- so that case keeps the original log-and-return behavior and
		// RunInteractive's outer loop goes back to blocking on the FIFO for the next command. A
		// net.Error with Timeout()==false (connection reset, broken pipe, etc.) -- or any other
		// non-net.Error failure, which doesn't match this benign timeout shape either -- means
		// the underlying game connection is actually gone, so it gets the same fatal treatment
		// as the adjacent SendExtension failure above: there's no point continuing to read from
		// the control FIFO once the connection -interactive exists to interact with is dead.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			slog.Error("no matching response within "+defaultCmdTimeout.String(), "error", err)
			return
		}
		slog.Error("response wait failed -- connection appears dead, exiting interactive mode", "error", err)
		os.Exit(1)
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
