package auth

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// loginUpdateForced is the Login reply's updateType for a forced update.
const loginUpdateForced = 2

// loginUpdateGate is what a Login reply says about the build it was claimed under.
type loginUpdateGate struct {
	updateType    int32
	hasUpdateType bool
	newVersion    string
}

// readLoginUpdateGate reads a Login reply's updateType (SFS Int) and new_version (UtfString), from
// the object that holds each (gsl.FindLoginReplyField: the reply, else its p). The real client
// stores updateType from every Login reply and hands new_version to the ShuMei SDK
// (LoginMessage.CSHandleResponse, Assembly-CSharp.decompiled.cs:82878-82896). Its GetInt can't
// read a non-integer updateType, so one is warned about and treated as absent.
func readLoginUpdateGate(content *sfs.SFSObject, context string) loginUpdateGate {
	var g loginUpdateGate
	if o := gsl.FindLoginReplyField(content, "updateType"); o != nil {
		if v, _ := o.Get("updateType"); session.SFSFieldKindAccepts(session.SFSFieldKindInt, v.Val) {
			g.updateType, g.hasUpdateType = o.GetInt("updateType"), true
		} else {
			slog.Warn("login reply: updateType present but not an integer; ignoring it", "context", context,
				"goType", fmt.Sprintf("%T", v.Val))
		}
	}
	if o := gsl.FindLoginReplyField(content, "new_version"); o != nil {
		session.WarnIfWrongTypedField(o, "new_version", context, session.SFSFieldKindString)
		g.newVersion = o.GetString("new_version")
	}
	return g
}

// logLoginUpdateGate logs a Login reply's updateType and new_version at Info, with the appVersion
// the Login claimed. The real client rewrites updateType 3 to 0 and goes on (:82888-82891); this
// logs the value as sent. With 2 it stops: the base Login enters LoadingState.AppUpdate instead of
// waiting for init (:13611-13613), and a cross-server Login fails with error 9 (:71194-71202).
// goclient doesn't enforce that yet and only warns. A new_version above the claimed one is no
// warning: the client uses it only to refuse new roles (ShumeiSdkManager.IsForbidCreateRole,
// :59785-59845).
func logLoginUpdateGate(content *sfs.SFSObject, context, appVersion string) {
	g := readLoginUpdateGate(content, context)
	updateType, newVersion := any("absent"), any("absent")
	if g.hasUpdateType {
		updateType = g.updateType
	}
	if g.newVersion != "" {
		newVersion = g.newVersion
	}
	slog.Info("login reply: update gate", "context", context, "updateType", updateType, "newVersion", newVersion, "appVersion", appVersion)
	if g.hasUpdateType && g.updateType == loginUpdateForced {
		slog.Warn("login reply: updateType 2, a forced update: the real client stops here and loads nothing, "+
			"but goclient doesn't enforce it yet and carries on; move the session's appVersion/versionCode to the current release",
			"context", context, "appVersion", appVersion, "newVersion", g.newVersion)
	}
}
