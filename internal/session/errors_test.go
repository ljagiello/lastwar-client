package session

import (
	"errors"
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

// TestLoginRejectionBlocked: every numeric LoginErrorCode is an *AccountBlockedError, and a ban's
// details come from inside the one ep string. The ban samples are discussion #53's packet: its §1
// banTime is below the int64 range, which only the sign check survives.
func TestLoginRejectionBlocked(t *testing.T) {
	cases := []struct {
		name string
		ep   string
		want AccountBlockedError
	}{
		{"permanent ban", "4;-9223372036847575810;13;703;Role",
			AccountBlockedError{Code: "4", Label: "ban", Permanent: true, BanTime: "-9223372036847575810", Reason: "703"}},
		{"ban time below int64", "4;-9223372036855575810;13;703;Role",
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

func TestLoginRejectionNotBlocked(t *testing.T) {
	cases := []struct {
		name string
		ep   []string
		want error
	}{
		{"rotated token", []string{"E011"}, ErrTokenRejected},
		{"identity mismatch", []string{"E005"}, ErrAuthRejected},
		{"deletion pending is an E code", []string{"E010"}, ErrAuthRejected},
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
		})
	}
}

func TestAccountBlockedErrorMessage(t *testing.T) {
	err := LoginRejection(loginReject("4;-9223372036847575810;13;703;Role"))
	const want = "ban, permanent, reason 703 (ep code 4): account or role blocked: server rejected authentication"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	err = LoginRejection(loginReject("4;1793000000000;13;Role"))
	const wantTimed = "ban, until 1793000000000 (ep code 4): account or role blocked: server rejected authentication"
	if err.Error() != wantTimed {
		t.Errorf("Error() = %q, want %q", err.Error(), wantTimed)
	}
}
