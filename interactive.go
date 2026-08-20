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
	"sync/atomic"
	"syscall"
	"time"
)

// interactiveShuttingDown is set by RunInteractive's signal-handling goroutine (below) before it
// calls conn.Close(), and checked by handleInteractiveLine's os.Exit(1) paths as well as
// RunInteractive's own 4 (round-43 fix, see below).
//
// Round-40 fix: SIGINT/SIGTERM during -interactive mode used to race two independent goroutines
// into calling os.Exit with contradictory codes. The signal handler calls conn.Close() then
// os.Exit(0); if the main goroutine happens to be blocked in handleInteractiveLine's
// SendExtension/waitForCmd calls at that moment, conn.Close() unblocks it with a genuine, non-
// timeout net.Error (confirmed live shape: conn_wait_test.go's
// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError) -- which handleInteractiveLine's own
// Timeout()-gated branch treats as "connection appears dead", logging a misleading message and
// calling os.Exit(1) from the main goroutine with no happens-before ordering against the signal
// goroutine's os.Exit(0). Whichever os.Exit call actually runs first is nondeterministic across
// otherwise-identical Ctrl-C events. Now the main goroutine checks this flag before treating a
// SendExtension/waitForCmd failure as a genuine dead-connection discovery: if a deliberate
// shutdown is already in progress, the failure is an expected side effect of that shutdown, not
// new information, so it returns quietly instead of racing to call its own os.Exit(1) -- the
// signal handler's os.Exit(0) is then the only exit call that ever happens, making the exit code
// deterministic on every Ctrl-C/SIGTERM.
//
// Round-43 fix: RunInteractive's own 4 fatal exit sites (control-pipe stat/non-FIFO/open failures,
// persistent scan-error give-up) had the identical unguarded-race gap this flag was built to
// prevent -- e.g. the control pipe being deleted/becoming inaccessible at the exact moment
// SIGTERM arrives during operator-initiated teardown. Each now checks this flag first and, if a
// shutdown is already in progress, returns from RunInteractive quietly instead of exiting: since
// RunInteractive's own for-loop never returns except via os.Exit, this lets the caller's normal
// exit-0 return path (or the signal goroutine's own imminent os.Exit(0)) be the only way the
// process actually terminates.
var interactiveShuttingDown atomic.Bool

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

// maxControlPipeLineSize is the maximum single control-FIFO line RunInteractive's bufio.Scanner
// will accept, overriding the default bufio.MaxScanTokenSize (64KB) that scanner would otherwise
// silently fall back to. This tool's documented command format (RunInteractive's own doc comment)
// is a bare cmd name plus a flat, scalar-only JSON params object -- nothing remotely close to this
// size -- so 1MB is generous headroom for a legitimate line while still bounding how much a single
// oversized/malformed line can make the scanner buffer before giving up with bufio.ErrTooLong,
// which -- like any other scanner.Err() -- now goes through the bounded consecutive-scan-error
// retry/backoff in RunInteractive below instead of being uniquely, immediately fatal.
const maxControlPipeLineSize = 1024 * 1024

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
		interactiveShuttingDown.Store(true)
		conn.Close()
		os.Exit(0)
	}()

	// consecutiveScanErrors tracks back-to-back loop iterations that ended via a genuine
	// scanner.Err() != nil (as opposed to a clean scanner.Scan()==false-with-nil-Err() EOF, the
	// ordinary "writer closed" case every iteration of this loop is otherwise designed around --
	// see the comment on that reopen below). Reset to 0 on any iteration that DOESN'T end in a scan
	// error, so this only fires on a genuinely persistent, back-to-back failure, not one isolated
	// blip in an otherwise-healthy stream of commands.
	consecutiveScanErrors := 0

	for {
		fi, statErr := statControlPipeWithRetry(controlPipe)
		if statErr != nil {
			// Round-43 fix: see interactiveShuttingDown's own doc comment -- this function's own 4
			// fatal exit sites (this one plus the not-a-FIFO, open-failure, and persistent-scan-error
			// sites below) never checked this flag, unlike handleInteractiveLine's two sites, so they
			// could still race the signal-handling goroutine's conn.Close();os.Exit(0) into a
			// nondeterministic 0-vs-1 exit code -- e.g. the control pipe being deleted/becoming
			// inaccessible at the exact moment SIGTERM arrives during operator-initiated teardown.
			// Returning quietly here (instead of exiting) lets the signal goroutine's os.Exit(0) be
			// the only exit call: this function then falls through to its caller's normal, exit-0
			// return path if that goroutine hasn't already terminated the process outright.
			if interactiveShuttingDown.Load() {
				return
			}
			slog.Error("stat control pipe failed", "controlPipe", controlPipe, "error", statErr, "retries", controlPipeRetries)
			// Round-41 fix: see the identical round-40 fix in main.go -- os.Exit does not run
			// deferred functions, so main()'s/runCrossServerTest()'s `defer conn.Close()`
			// (registered before RunInteractive is called, which blocks until it exits the
			// process) never ran on any of this function's or handleInteractiveLine's os.Exit(1)
			// paths. Close explicitly before exiting instead of relying on that now-unreachable
			// defer.
			conn.Close()
			os.Exit(1)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			// Not retried, unlike the stat/open failures above/below: a path that exists but isn't
			// a FIFO is a permanent misconfiguration (-interactive pointed at a plain file, or
			// mkfifo was simply never run), not a transient condition that resolves itself if we
			// just wait and look again.
			// See the round-43 fix comment on the stat-failure branch above -- same
			// interactiveShuttingDown guard, same reasoning.
			if interactiveShuttingDown.Load() {
				return
			}
			slog.Error("controlPipe exists but is not a FIFO -- did you forget mkfifo?", "controlPipe", controlPipe)
			conn.Close()
			os.Exit(1)
		}
		f, err := openControlPipeWithRetry(controlPipe)
		if err != nil {
			// See the round-43 fix comment on the stat-failure branch above -- same
			// interactiveShuttingDown guard, same reasoning.
			if interactiveShuttingDown.Load() {
				return
			}
			slog.Error("open control pipe failed", "controlPipe", controlPipe, "error", err, "retries", controlPipeRetries)
			conn.Close()
			os.Exit(1)
		}
		scanner := bufio.NewScanner(f)
		// See controlPipeRetries' own doc comment for why a bufio.Scanner's default 64KB
		// bufio.MaxScanTokenSize would otherwise turn any single line over that size into a fatal
		// bufio.ErrTooLong scan error, indistinguishable here from a genuinely broken pipe. Sized
		// generously above the flat scalar-only command-line format this tool actually supports
		// (see RunInteractive's own doc comment) -- a legitimate line is always far smaller than
		// this -- purely so an oversized line is treated the same as any other persistent scan
		// error below (bounded retry, then give up) instead of being uniquely fatal on its very
		// first occurrence.
		scanner.Buffer(make([]byte, 0, 64*1024), maxControlPipeLineSize)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			handleInteractiveLine(conn, line)
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			consecutiveScanErrors++
			slog.Error("control pipe scan error", "controlPipe", controlPipe, "error", err, "consecutiveScanErrors", consecutiveScanErrors)
			if consecutiveScanErrors > controlPipeRetries {
				// Same bound as statControlPipeWithRetry/openControlPipeWithRetry's own
				// controlPipeRetries budget above -- a scan error persisting across this many
				// back-to-back reopen attempts, with no delay between them, is exactly the
				// unbounded open-error-close spin this fix exists to prevent: treat it as
				// genuinely fatal instead of looping forever.
				// See the round-43 fix comment on the stat-failure branch above -- same
				// interactiveShuttingDown guard, same reasoning: a persistent scan error caused by
				// the signal goroutine's own conn.Close()/FIFO teardown mid-shutdown is an expected
				// side effect, not a new fatal discovery.
				if interactiveShuttingDown.Load() {
					return
				}
				slog.Error("control pipe scan error persisted too many times in a row, giving up", "controlPipe", controlPipe, "consecutiveScanErrors", consecutiveScanErrors)
				conn.Close()
				os.Exit(1)
			}
			// A writer still connected and producing bad input (e.g. one that keeps reopening the
			// FIFO and immediately erroring) could otherwise spin this loop with no delay at all --
			// pause before reopening, mirroring controlPipeRetryDelay's own pacing.
			time.Sleep(controlPipeRetryDelay)
			continue
		}
		// A clean scanner.Scan()==false with a nil Err() is the ordinary case: a FIFO reader sees
		// EOF once every writer closes. Reset the consecutive-error counter (this iteration wasn't
		// a scan error at all) and reopen, waiting for the next command instead of exiting.
		consecutiveScanErrors = 0
	}
}

// statControlPipe/openControlPipeFile indirect os.Stat/os.Open through package vars, mirroring
// login.go's dialGame seam -- so a test can substitute a counting wrapper to prove
// statControlPipeWithRetry/openControlPipeWithRetry make EXACTLY controlPipeRetries+1 attempts
// before giving up (round 49 regression test for the MINOR finding that only a generous
// wall-clock upper bound was ever asserted, not the exact attempt count -- a future edit changing
// either loop's `attempt <= controlPipeRetries`/`attempt < controlPipeRetries` to a `<`/`<=`
// off-by-one would silently give up one attempt early and go undetected, since the ~50ms
// difference is invisible against a multi-second wall-clock bound). Production code always
// resolves these to the real os.Stat/os.Open; only tests ever reassign them.
var statControlPipe = os.Stat
var openControlPipeFile = os.Open

// statControlPipeWithRetry stats controlPipe, retrying up to controlPipeRetries times (with a
// controlPipeRetryDelay pause between attempts) before giving up -- see controlPipeRetries' own
// doc comment for why a single os.Stat failure here must not be immediately fatal the way it used
// to be. Returns the last error seen if every attempt fails.
func statControlPipeWithRetry(controlPipe string) (os.FileInfo, error) {
	var fi os.FileInfo
	var err error
	for attempt := 0; attempt <= controlPipeRetries; attempt++ {
		fi, err = statControlPipe(controlPipe)
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
		f, err = openControlPipeFile(controlPipe)
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
		if raw == nil {
			// A bare top-level JSON "null" decodes into a map[string]any successfully -- Decode
			// leaves raw as the type's zero value (nil) rather than erroring -- unlike every OTHER
			// malformed-body shape (a bare number/string/bool/array), which all correctly fail this
			// Decode call and already hit the error branch just above. Left unchecked, this one
			// shape would slip through as if it were a legitimate empty params object -- silently
			// sending the command with no params and no diagnostic, instead of surfacing what's
			// almost certainly a mistake (e.g. a stray "null" left over from copy-pasting a
			// response body as if it were a request).
			slog.Error("bad JSON params", "cmd", cmd, "error", fmt.Errorf("params decoded to JSON null, not an object"))
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
		// See interactiveShuttingDown's own doc comment: a send failure caused by RunInteractive's
		// own signal-handling goroutine already closing this connection is an expected side effect
		// of that shutdown, not a new dead-connection discovery -- return quietly instead of racing
		// that goroutine's os.Exit(0) with a second, contradictory os.Exit(1) call.
		if interactiveShuttingDown.Load() {
			return
		}
		// sendStageError: consistency with conn.go's heartbeat/sendAndWait/DoHandshake and the
		// login-path send sites (round 48) -- this error is never returned across a function
		// boundary or inspected for Timeout() today (it's logged and the connection is closed
		// unconditionally either way), so wrapping it has no behavioral effect now, but keeps
		// the invariant "every direct send-stage error in this package is sendStageError-
		// wrapped" true package-wide for any future caller that does inspect it (e.g. if this
		// were ever changed to only fatally exit on a genuine non-timeout net.Error, mirroring
		// the Timeout()-gated check the very next lines already apply to waitForCmd's error).
		slog.Error("send failed -- connection appears dead, exiting interactive mode", "error", sendStageError{err: err})
		// See RunInteractive's own round-41 fix doc comment: os.Exit skips the caller's `defer
		// conn.Close()`, so close explicitly before exiting instead of relying on it.
		conn.Close()
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
		// See interactiveShuttingDown's own doc comment above -- same reasoning as the adjacent
		// SendExtension guard: a non-timeout net.Error here is the expected shape of conn.Close()
		// unblocking this wait mid-shutdown, not a new dead-connection discovery.
		if interactiveShuttingDown.Load() {
			return
		}
		slog.Error("response wait failed -- connection appears dead, exiting interactive mode", "error", err)
		// See the identical round-41 fix's doc comment on the sibling os.Exit(1) above.
		conn.Close()
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
			// Round-41 fix: this used to log val.String() unconditionally, bypassing every
			// redaction layer this file otherwise enforces for operator-typed FIFO input --
			// contradicting the neighboring JSON decode-error/trailing-data branches, which
			// explicitly avoid echoing raw operator text specifically because "the glued-on JSON
			// could contain a credential value the operator meant to pass as params" (see
			// handleInteractiveLine's own doc comments). An out-of-both-int64-and-float64-range
			// JSON number literal (e.g. 1e400) under a sensitive key name (loginKey, accessToken,
			// etc.) used to reach this branch and log its raw value in cleartext, unlike a
			// successfully-parsed value, which only ever reaches the wire via params.StringRedacted().
			logVal := val.String()
			if isSensitiveSFSKey(key) {
				logVal = "[REDACTED]"
			}
			slog.Error("unparseable JSON number", "key", key, "value", logVal)
			return false
		}
	default:
		slog.Error("unsupported JSON value type", "key", key, "type", fmt.Sprintf("%T", v))
		return false
	}
	return true
}
