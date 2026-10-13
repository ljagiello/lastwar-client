package auth

import (
	"fmt"
	"lastwar-client/internal/sfs"
	"log/slog"
	"strconv"
	"strings"
)

// accountRole is one entry of push.account.login.new's accountArr: a role the verified email owns,
// on one server. Confirmed live on 2026-10-13 (a one-role account), an entry carries the server's
// id, zone, name and gateways, the role's gameUid, gameUserName, gameUserLevel and power, and also
// the device uuid the role was created on and the server's internal database hosts; it carried no
// loginKey (that one is the account's, at the push's top level). Only the fields below are kept.
//
// The 1.0.364 role picker (UIRoleLoginView.OnClickLogin, docs/live-validation.mdx) takes the
// picked entry's ip/port/zone, loginKey and gameUid, then logs in through GSL opt=login. So an
// entry's own loginKey wins when one is there; binding a role other than the top-level one is
// static analysis only, never tried live.
type accountRole struct {
	ServerID   string // the entry's id, e.g. "783"
	Zone       string // e.g. "APS783"
	ServerName string // e.g. "State#783"
	GameUid    string
	Name       string // gameUserName: PII, never logged
	LoginKey   string // the entry's own loginKey; "" in the live one-role reply
	Level      string
	Power      string
}

// accountRoles reads the roles in push.account.login.new's accountArr, skipping entries that
// aren't objects.
func accountRoles(push *sfs.SFSObject) []accountRole {
	v, ok := push.Get("accountArr")
	if !ok {
		return nil
	}
	arr, ok := v.Val.(*sfs.SFSArray)
	if !ok {
		slog.Warn("push.account.login.new: accountArr is not an array", "goType", fmt.Sprintf("%T", v.Val))
		return nil
	}
	var roles []accountRole
	for _, item := range arr.Items() {
		o, ok := item.Val.(*sfs.SFSObject)
		if !ok {
			continue
		}
		roles = append(roles, accountRole{
			ServerID: scalarString(o, "id"), Zone: o.GetString("zone"), ServerName: o.GetString("name"),
			GameUid: o.GetString("gameUid"), Name: o.GetString("gameUserName"), LoginKey: o.GetString("loginKey"),
			Level: scalarString(o, "gameUserLevel"), Power: scalarString(o, "power"),
		})
	}
	return roles
}

// scalarString returns a string, int or long field as a string, "" otherwise.
func scalarString(o *sfs.SFSObject, key string) string {
	v, ok := o.Get(key)
	if !ok {
		return ""
	}
	switch val := v.Val.(type) {
	case string:
		return val
	case int16:
		return strconv.Itoa(int(val))
	case int32:
		return strconv.Itoa(int(val))
	case int64:
		return strconv.FormatInt(val, 10)
	}
	return ""
}

// logAccountRoles lists the roles at Info so an operator can pick one with -server.
func logAccountRoles(roles []accountRole) {
	slog.Info("account roles", "count", len(roles))
	for i, r := range roles {
		slog.Info("account role", "index", i, "server", r.ServerID, "zone", r.Zone, "serverName", r.ServerName,
			"gameUid", r.GameUid, "level", r.Level, "power", r.Power, "hasOwnLoginKey", r.LoginKey != "")
	}
}

// serverNumber reduces a server given as "783", "APS783" or "aps783" to "783".
func serverNumber(server string) string {
	s := strings.TrimSpace(server)
	if len(s) >= 3 && strings.EqualFold(s[:3], "APS") {
		s = s[3:]
	}
	return s
}

// validServer checks a -server value ("783" or "APS783") before an email code is spent on it.
func validServer(server string) error {
	n := serverNumber(server)
	if n == "" {
		return fmt.Errorf("-server %q names no server number", server)
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return fmt.Errorf("-server %q is not a server number like 783 or a zone like APS783", server)
		}
	}
	return nil
}

// pickRole returns the one role on server, given as "783" or "APS783" and matched against the
// entry's id or zone. It fails when no role or more than one is there, or when the role has no
// gameUid; the error names the servers the account has roles on.
func pickRole(roles []accountRole, server string) (accountRole, error) {
	want := serverNumber(server)
	var matches []accountRole
	have := make([]string, 0, len(roles))
	for _, r := range roles {
		id := r.ServerID
		if id == "" {
			id = serverNumber(r.Zone)
		}
		have = append(have, id)
		if want != "" && (r.ServerID == want || (r.Zone != "" && serverNumber(r.Zone) == want)) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return accountRole{}, fmt.Errorf("no role on server %s; the account has roles on servers %v", want, have)
	case 1:
	default:
		uids := make([]string, len(matches))
		for i, m := range matches {
			uids[i] = m.GameUid
		}
		return accountRole{}, fmt.Errorf("%d roles on server %s (gameUids %v); can't tell which to bind", len(matches), want, uids)
	}
	if matches[0].GameUid == "" {
		return accountRole{}, fmt.Errorf("the role on server %s has no gameUid", want)
	}
	return matches[0], nil
}
