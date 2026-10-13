package auth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// liveShapeRole builds an accountArr entry with the fields a real one carried on 2026-10-13
// (a subset: the server, the role, and the device uuid and internal host that must not be logged).
// id is an Int here; a String id is covered separately.
func liveShapeRole(id int32, zone, gameUid, name string) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutInt("id", id)
	o.PutUtfString("zone", zone)
	o.PutUtfString("name", "State#"+zone[3:])
	o.PutUtfString("gameUid", gameUid)
	o.PutUtfString("gameUserName", name)
	o.PutInt("gameUserLevel", 35)
	o.PutLong("power", 247419474)
	o.PutUtfString("uuid", "secret-device-uuid_n3d1722738235319")
	o.PutUtfString("db_ip", "game-db-internal.example")
	o.PutUtfString("ip", "gw-a.example|gw-b.example")
	return o
}

func pushWithRoles(roles ...*sfs.SFSObject) *sfs.SFSObject {
	arr := sfs.NewSFSArray()
	for _, r := range roles {
		arr.AddSFSObject(r)
	}
	p := sfs.NewSFSObject()
	p.PutUtfString("loginKey", "account-login-key")
	p.PutUtfString("gameUid", "uid-783")
	p.PutSFSArray("accountArr", arr)
	return p
}

func TestAccountRolesReadsTheLiveShape(t *testing.T) {
	str := sfs.NewSFSObject()
	str.PutUtfString("id", "72")
	str.PutUtfString("zone", "APS72")
	str.PutUtfString("gameUid", "uid-72")
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(liveShapeRole(783, "APS783", "uid-783", "Commander"))
	arr.AddInt(5) // not an object: skipped
	arr.AddSFSObject(str)
	p := sfs.NewSFSObject()
	p.PutSFSArray("accountArr", arr)

	roles := accountRoles(p)
	if len(roles) != 2 {
		t.Fatalf("roles = %d, want 2", len(roles))
	}
	if r := roles[0]; r.ServerID != "783" || r.Zone != "APS783" || r.GameUid != "uid-783" || r.Name != "Commander" {
		t.Errorf("role 0 = %q/%q/%q/%q", r.ServerID, r.Zone, r.GameUid, r.Name)
	}
	if r := roles[1]; r.ServerID != "72" || r.GameUid != "uid-72" {
		t.Errorf("role 1 = %q/%q, want a String id read too", r.ServerID, r.GameUid)
	}
	if accountRoles(sfs.NewSFSObject()) != nil {
		t.Error("no accountArr should give no roles")
	}
	notArr := sfs.NewSFSObject()
	notArr.PutUtfString("accountArr", "x")
	if accountRoles(notArr) != nil {
		t.Error("a non-array accountArr should give no roles")
	}
}

func TestPickRole(t *testing.T) {
	roles := accountRoles(pushWithRoles(
		liveShapeRole(783, "APS783", "uid-783", "A"),
		liveShapeRole(72, "APS72", "uid-72", "B"),
	))
	for _, server := range []string{"783", "APS783", "aps783", " 783 "} {
		r, err := pickRole(roles, server)
		if err != nil || r.GameUid != "uid-783" {
			t.Errorf("pickRole(%q) = %q, %v; want uid-783", server, r.GameUid, err)
		}
	}
	if r, err := pickRole(roles, "72"); err != nil || r.GameUid != "uid-72" {
		t.Errorf("pickRole(72) = %q, %v", r.GameUid, err)
	}

	_, err := pickRole(roles, "999")
	if err == nil || !strings.Contains(err.Error(), "[783 72]") {
		t.Errorf("no match: err = %v, want the account's servers listed", err)
	}
	if _, err := pickRole(roles, ""); err == nil {
		t.Error("an empty server must not match a role")
	}

	dup := accountRoles(pushWithRoles(liveShapeRole(783, "APS783", "uid-a", "A"), liveShapeRole(783, "APS783", "uid-b", "B")))
	if _, err := pickRole(dup, "783"); err == nil || !strings.Contains(err.Error(), "uid-a") {
		t.Errorf("two roles on one server: err = %v, want both gameUids named", err)
	}

	noUid := accountRoles(pushWithRoles(liveShapeRole(783, "APS783", "", "A")))
	if _, err := pickRole(noUid, "783"); err == nil {
		t.Error("a role without a gameUid can't be bound")
	}

	// An entry without an id is matched by its zone.
	z := sfs.NewSFSObject()
	z.PutUtfString("zone", "APS8092")
	z.PutUtfString("gameUid", "uid-8092")
	if r, err := pickRole(accountRoles(pushWithRoles(z)), "8092"); err != nil || r.GameUid != "uid-8092" {
		t.Errorf("zone-only entry: %q, %v", r.GameUid, err)
	}
}

func TestValidServerRejectsNonNumbers(t *testing.T) {
	for _, ok := range []string{"783", "APS783", "aps8092", " 72 "} {
		if err := validServer(ok); err != nil {
			t.Errorf("validServer(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"APS", "", "  ", "State#783", "78x", "APS-1"} {
		if err := validServer(bad); err == nil {
			t.Errorf("validServer(%q) = nil, want an error", bad)
		}
	}
}

// TestAccountRolesReadsAnEntrysOwnLoginKey: the live one-role reply had no per-entry loginKey, but
// the 1.0.364 role picker reads param.loginKey, so an entry that carries one keeps it.
func TestAccountRolesReadsAnEntrysOwnLoginKey(t *testing.T) {
	e := liveShapeRole(72, "APS72", "uid-72", "Alt")
	e.PutUtfString("loginKey", "lk-role-72")
	roles := accountRoles(pushWithRoles(liveShapeRole(783, "APS783", "uid-783", "Main"), e))
	if roles[0].LoginKey != "" || roles[1].LoginKey != "lk-role-72" {
		t.Errorf("entry loginKeys = %q/%q, want \"\"/lk-role-72", roles[0].LoginKey, roles[1].LoginKey)
	}
}

// TestPickRoleMatchesEitherField: an entry is matched by its id alone (Int, Short or String) or by
// its zone alone, and the no-match error lists a zone-only entry by its zone's number.
func TestPickRoleMatchesEitherField(t *testing.T) {
	idOnly := sfs.NewSFSObject()
	idOnly.PutUtfString("id", "72")
	idOnly.PutUtfString("gameUid", "uid-72")
	short := sfs.NewSFSObject()
	short.PutShort("id", 15)
	short.PutUtfString("gameUid", "uid-15")
	zoneOnly := sfs.NewSFSObject()
	zoneOnly.PutUtfString("zone", "APS8092")
	zoneOnly.PutUtfString("gameUid", "uid-8092")
	roles := accountRoles(pushWithRoles(idOnly, short, zoneOnly))
	for server, want := range map[string]string{"72": "uid-72", "15": "uid-15", "APS8092": "uid-8092"} {
		if r, err := pickRole(roles, server); err != nil || r.GameUid != want {
			t.Errorf("pickRole(%q) = %q, %v; want %s", server, r.GameUid, err, want)
		}
	}
	if _, err := pickRole(roles, "1"); err == nil || !strings.Contains(err.Error(), "[72 15 8092]") {
		t.Errorf("err = %v, want the servers listed, the zone-only one by number", err)
	}
}

// TestOldUidIsRedacted: an accountArr entry's oldUid is the deviceId the role was created on, so a
// redacted dump of the push hides it.
func TestOldUidIsRedacted(t *testing.T) {
	e := liveShapeRole(783, "APS783", "uid-783", "Main")
	e.PutUtfString("oldUid", "secret-old-device_n3d1")
	if out := pushWithRoles(e).StringRedacted(); strings.Contains(out, "secret-old-device_n3d1") {
		t.Errorf("StringRedacted shows oldUid: %s", out)
	}
}

// TestLogAccountRolesOmitsDeviceAndHosts: the role list shows the server and role, never the
// device uuid, internal hosts, or the role's name.
func TestLogAccountRolesOmitsDeviceAndHosts(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	e := liveShapeRole(783, "APS783", "uid-783", "SecretName")
	e.PutUtfString("loginKey", "lk-secret-role")
	logAccountRoles(accountRoles(pushWithRoles(e)))
	out := buf.String()
	for _, want := range []string{"server=783", "zone=APS783", "gameUid=uid-783", "level=35", "power=247419474", "serverName=State#783", "hasOwnLoginKey=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	for _, leak := range []string{"secret-device-uuid", "game-db-internal", "SecretName", "lk-secret-role"} {
		if strings.Contains(out, leak) {
			t.Errorf("log contains %q:\n%s", leak, out)
		}
	}
}
