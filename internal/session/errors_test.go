package session

import (
	"errors"
	"net"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// loginReject is a rejected Login's content: ec=28 and ep as the server sends it, a string array.
func loginReject(ep ...string) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	if ep != nil {
		o.PutValue("ep", sfs.SFSValue{Type: 16, Val: ep}) // 16 = UTF_STRING_ARRAY
	}
	o.PutShort("ec", 28)
	return o
}

// TestLoginRejectionBlocked: a ban's details come from inside the one ep string. The ban samples
// are the two ban times quoted in discussion #53.
func TestLoginRejectionBlocked(t *testing.T) {
	cases := []struct {
		name string
		ep   string
		want AccountBlockedError
	}{
		{"permanent ban", "4;-9223372036847575810;13;703;Role",
			AccountBlockedError{Code: "4", Label: "ban", Permanent: true, BanTime: "-9223372036847575810", Reason: "703"}},
		{"#53's other ban time", "4;-9223372036855575810;13;703;Role",
			AccountBlockedError{Code: "4", Label: "ban", Permanent: true, BanTime: "-9223372036855575810", Reason: "703"}},
		{"all-time ban", "4;9223372036854775806;13;706;Role",
			AccountBlockedError{Code: "4", Label: "ban", Permanent: true, BanTime: "9223372036854775806", Reason: "706"}},
		{"timed ban", "4;1793000000000;13;727;Role",
			AccountBlockedError{Code: "4", Label: "ban", BanTime: "1793000000000", Reason: "727"}},
		{"four-field ban has no reason", "4;-1;13;Role",
			AccountBlockedError{Code: "4", Label: "ban", Permanent: true, BanTime: "-1"}},
		{"bare ban", "4", AccountBlockedError{Code: "4", Label: "ban"}},
		{"device blacklist", "44", AccountBlockedError{Code: "44", Label: "device blacklist"}},
		{"traffic control", "49;600;x", AccountBlockedError{Code: "49", Label: "traffic control ban"}},
		{"role deletion", "58", AccountBlockedError{Code: "58", Label: "role deletion confirmed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := LoginRejection(loginReject(c.ep))
			var got *AccountBlockedError
			if !errors.As(err, &got) {
				t.Fatalf("LoginRejection(%q) = %v, want an *AccountBlockedError", c.ep, err)
			}
			if *got != c.want {
				t.Errorf("LoginRejection(%q) = %+v, want %+v", c.ep, *got, c.want)
			}
			if !errors.Is(err, ErrAccountBlocked) || !errors.Is(err, ErrAuthRejected) {
				t.Errorf("errors.Is(%v, ErrAccountBlocked/ErrAuthRejected) = false", err)
			}
			if errors.Is(err, ErrTokenRejected) {
				t.Errorf("errors.Is(%v, ErrTokenRejected) = true; a blocked account must not trigger a token refresh", err)
			}
		})
	}
}

// TestLoginRejectionEveryBlockedCode: each numeric code the docs promise exit 4 for, written out
// rather than read from blockedLoginCodes, so dropping one from the table fails here.
func TestLoginRejectionEveryBlockedCode(t *testing.T) {
	for _, code := range []string{"4", "43", "44", "45", "46", "47", "48", "49", "55", "56", "57", "58"} {
		if err := LoginRejection(loginReject(code)); !errors.Is(err, ErrAccountBlocked) {
			t.Errorf("LoginRejection(%q) = %v, want ErrAccountBlocked", code, err)
		}
	}
}

func TestLoginRejectionNotBlocked(t *testing.T) {
	cases := []struct {
		name string
		ep   []string
		want error
	}{
		{"rotated token", []string{"E011"}, ErrTokenRejected},
		{"identity mismatch or maintenance", []string{"E005"}, ErrLoginMaintenanceOrIdentity},
		{"deletion pending is an E code", []string{"E010"}, ErrAuthRejected},
		// E013 and E014 wipe the client's user data (Assembly-CSharp.decompiled.cs:13463-13473,
		// 13492-13495), E001/E002 its auth (13335-13339): all stay "recapture".
		{"season registration ban", []string{"E013"}, ErrAuthRejected},
		{"old uid, new refresh token", []string{"E014"}, ErrAuthRejected},
		{"account does not exist", []string{"E001"}, ErrAuthRejected},
		{"unlisted number", []string{"40"}, ErrAuthRejected},
		{"empty ep", []string{}, ErrAuthRejected},
		{"no ep", nil, ErrAuthRejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := LoginRejection(loginReject(c.ep...))
			if err != c.want {
				t.Errorf("LoginRejection(%q) = %v, want %v", c.ep, err, c.want)
			}
			if !errors.Is(err, ErrAuthRejected) {
				t.Errorf("errors.Is(%v, ErrAuthRejected) = false; main.go would not exit 2", err)
			}
		})
	}
}

// TestLoginRejectionE005StaysExit2 (B67): E005 keeps exit 2 until the owner decides (MASTER 6.1),
// and its text says the client shows it as maintenance (LoginState.OnLoginResponse maps it to E109,
// Assembly-CSharp.decompiled.cs:13627-13628).
func TestLoginRejectionE005StaysExit2(t *testing.T) {
	err := LoginRejection(loginReject("E005"))
	if !errors.Is(err, ErrAuthRejected) || errors.Is(err, ErrSeasonWindow) {
		t.Errorf("LoginRejection(E005) = %v, want an ErrAuthRejected and not a season window", err)
	}
	if !strings.Contains(err.Error(), "maintenance") || !strings.Contains(err.Error(), "E109") {
		t.Errorf("LoginRejection(E005) = %q, want it to say the client shows it as maintenance (E109)", err)
	}
}

// TestLoginRejectionSeasonWindow (B67): E015 (ERROR_SEASON_WILL_START) and E016
// (ERROR_SEASON_STOP_MAINTENANCE) are a server state the client meets with a reload-or-quit box
// and keeps every credential (Assembly-CSharp.decompiled.cs:13474-13491, 13648-13653,
// 220289-220291). They must not read as an auth rejection (exit 2, "recapture"), nor as a net
// error that would send main.go's connectFailed to GSL for another address.
func TestLoginRejectionSeasonWindow(t *testing.T) {
	for _, code := range []string{"E015", "E016"} {
		err := LoginRejection(loginReject(code))
		if !errors.Is(err, ErrSeasonWindow) {
			t.Errorf("LoginRejection(%s) = %v, want ErrSeasonWindow", code, err)
		}
		if errors.Is(err, ErrAuthRejected) || errors.Is(err, ErrTokenRejected) || errors.Is(err, ErrAccountBlocked) {
			t.Errorf("LoginRejection(%s) = %v wraps an auth sentinel; main.go would exit 2 or 4", code, err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) {
			t.Errorf("LoginRejection(%s) = %v is a net.Error; connectFailed would look the server up again", code, err)
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("LoginRejection(%s) = %q, want the code in the text", code, err)
		}
	}
}

func TestAccountBlockedErrorMessage(t *testing.T) {
	err := LoginRejection(loginReject("4;-9223372036847575810;13;703;Role"))
	want := "ban, permanent, reason 703 (ep code 4): " + ErrAccountBlocked.Error()
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	err = LoginRejection(loginReject("4;1793000000000;13;Role"))
	wantTimed := "ban, until 1793000000000 (ep code 4): " + ErrAccountBlocked.Error()
	if err.Error() != wantTimed {
		t.Errorf("Error() = %q, want %q", err.Error(), wantTimed)
	}
}
