package game

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Mail type 112 (CANT_CLAIM_ALL) rewards, step 1: log only (OPT-IN, read-only; MASTER 1.0.370 G53;
// aud-dailies F5 and its verdict). Static-only: from the 1.0.370 Lua, not run live yet.
//
// The client's Claim All leaves MailCantClaimAllType out (112, and the translation-rating mail
// 134/135; MailEnum.lua:53, 282-286; MailDBManager.lua:1061-1075), and so does ClaimAllMail
// (mailCantClaimAllTypes). A 112 mail is claimed only from its own button: UILWMailMainView
// OnReceiveClick (UILWMailMainView.lua:683-687) -> ReceiveCurrentMail (UILWMailMainCtrl.lua:208-220)
// -> MailDataManager:RewardMail (MailDataManager.lua:710-721) -> `mail.reward.batch {uids: UtfString
// one uid, type: UtfString group "5"}` (MailRewardBatchMessage.lua:3-17; 112 is group 5,
// mailTypeGroup). The client gives no reason why 112 is kept out of Claim All, and that reason
// decides whether claiming it is allowed (update-to-claim, link or binding mail would sit in exactly
// such a type, and those are never automated). So this feature claims nothing: it logs every unsaved
// 112 mail with rewardStatus 0 (uid, title, and the reward's rewardVersion / rewardLevel from the
// mail body's b.reward, MailInfo.lua:96-105, 287-325), so a live sample can show what these mails
// are before a claim is built. Never 134/135 (rating mail). It reads the mailbox the core
// ClaimAllMail already listed on this connection (mailListFor) and lists it only when that did not
// happen.
const (
	mailSingleClaimsFeature = "mail-single-claims-plan"
	mailCantClaimAllType    = 112 // MailType.CANT_CLAIM_ALL (MailEnum.lua:53)
	mailSingleTitleMax      = 160 // log at most this much of a title
)

func init() {
	registerFeature(Feature{
		Name: mailSingleClaimsFeature,
		Summary: "read-only: log the unclaimed reward mail of type 112 (CANT_CLAIM_ALL) that Claim All leaves out, " +
			"as a sample for a later single-mail claim; sends no claim (static-only, not validated live)",
		Run: runMailSingleClaimsPlan,
	})
}

// mailContents is MailInfo:ParseBaseData's contents: contentsArr joined with no separator, else
// contentsLocal, else contents (MailInfo.lua:96-105; string.join is table.concat,
// Common/StringUtil.lua:35-37).
func mailContents(m Mail) string {
	if v, ok := m.Raw.Get("contentsArr"); ok && v.Val != nil {
		var b strings.Builder
		switch a := v.Val.(type) {
		case *sfs.SFSArray:
			for _, it := range a.Items() {
				if s, ok := it.Val.(string); ok {
					b.WriteString(s)
				}
			}
		case []string:
			for _, s := range a {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	if s := m.Raw.GetString("contentsLocal"); s != "" {
		return s
	}
	return m.Raw.GetString("contents")
}

// mailCantClaimAllCandidates returns the mail a single-mail claim of type 112 would look at:
// type 112, rewardStatus present and 0 (HasUnclaimedReward) and not saved (saveFlag 0).
func mailCantClaimAllCandidates(mail []Mail) []Mail {
	var out []Mail
	for _, m := range mail {
		if t, ok := claimInt(m.Raw, "type"); !ok || t != mailCantClaimAllType {
			continue
		}
		if f, ok := claimInt(m.Raw, "saveFlag"); ok && f != 0 {
			continue
		}
		if !m.HasUnclaimedReward() {
			continue
		}
		out = append(out, m)
	}
	return out
}

func runMailSingleClaimsPlan(conn *session.GameConn, in *Init) error {
	mail, err := mailListFor(conn)
	if err != nil {
		return fmt.Errorf("%s: list mail: %w", mailSingleClaimsFeature, err)
	}
	found := mailCantClaimAllCandidates(mail)
	if len(found) == 0 {
		slog.Info(mailSingleClaimsFeature+": no unclaimed type-112 mail", "totalMail", len(mail))
		return nil
	}
	for _, m := range found {
		contents := mailContents(m)
		var body struct {
			B struct {
				Reward map[string]any `json:"reward"`
			} `json:"b"`
		}
		parsed := json.Unmarshal([]byte(contents), &body) == nil
		ver, hasVer := body.B.Reward["rewardVersion"]
		title := m.Raw.GetString("title")
		if len(title) > mailSingleTitleMax {
			title = title[:mailSingleTitleMax] + "..."
		}
		// rewardVersion / rewardLevel are what the old mail view's ShowRewardBtn tests before it
		// offers a claim (a version newer than the app's, or an HQ below the level, holds it back;
		// noticeClaimable applies the same rule to notices), logged for the step-2 decision.
		slog.Info(mailSingleClaimsFeature+": unclaimed type-112 mail left for the player",
			"uid", m.Uid(), "title", title, "bodyParsed", parsed, "hasReward", body.B.Reward != nil,
			"hasRewardVersion", hasVer, "rewardVersion", ver, "rewardLevel", body.B.Reward["rewardLevel"],
			"hqLevel", activityHQLevel(in), "createTime", m.Raw.GetLong("createTime"))
	}
	slog.Info(mailSingleClaimsFeature+": type-112 mail not claimed (log only)", "count", len(found))
	return nil
}
