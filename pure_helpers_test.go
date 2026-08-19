package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestCollectibleBuildings(t *testing.T) {
	newBuilding := func(uuid int64, bId int32) Building {
		o := NewSFSObject()
		o.PutLong("uuid", uuid)
		o.PutInt("bId", bId)
		return Building{Raw: o}
	}
	buildings := []Building{
		newBuilding(1, BuildingFarmland),
		newBuilding(2, 99999999), // not a recognized collectible type
		newBuilding(3, BuildingIronMine),
	}
	got := collectibleBuildings(buildings)
	if len(got) != 2 {
		t.Fatalf("got %d collectible buildings, want 2", len(got))
	}
	if got[0].Uuid() != 1 || got[1].Uuid() != 3 {
		t.Errorf("unexpected uuids: %d, %d", got[0].Uuid(), got[1].Uuid())
	}
}

func TestGroupUnclaimedByType(t *testing.T) {
	newMail := func(uid string, mailType int32, rewardStatus int32) Mail {
		o := NewSFSObject()
		o.PutUtfString("uid", uid)
		o.PutInt("type", mailType)
		o.PutInt("rewardStatus", rewardStatus)
		return Mail{Raw: o}
	}
	mail := []Mail{
		newMail("a", 3, 0), // unclaimed, type 3
		newMail("b", 3, 1), // already claimed
		newMail("c", 4, 0), // unclaimed, type 4
		newMail("d", 3, 0), // unclaimed, type 3
	}
	got := groupUnclaimedByType(mail)
	if len(got) != 2 {
		t.Fatalf("got %d distinct types, want 2", len(got))
	}
	if len(got[3]) != 2 || got[3][0] != "a" || got[3][1] != "d" {
		t.Errorf("type 3: got %v, want [a d]", got[3])
	}
	if len(got[4]) != 1 || got[4][0] != "c" {
		t.Errorf("type 4: got %v, want [c]", got[4])
	}
}

// TestHasUnclaimedRewardMissingFieldIsNotUnclaimed guards the explicit-null-vs-missing fix to
// HasUnclaimedReward: mail with a genuinely-absent rewardStatus key (notification-only mail --
// alliance markers, battle reports, and similar, per ClaimAllMail's doc comment) must NOT be
// treated as unclaimed, even though GetInt("rewardStatus") returns the same int32 zero value for
// "genuinely absent" as it does for a real rewardStatus=0. Reverting the presence check in
// HasUnclaimedReward (back to a bare `RewardStatus() == 0`) would make this fail, since the
// no-rewardStatus mail would then be misclassified as unclaimed and swept into byType.
func TestHasUnclaimedRewardMissingFieldIsNotUnclaimed(t *testing.T) {
	noRewardStatus := NewSFSObject()
	noRewardStatus.PutUtfString("uid", "notif-1")
	noRewardStatus.PutInt("type", 3)
	// deliberately no PutInt("rewardStatus", ...) call -- field genuinely absent, as opposed to
	// an explicit 0.

	withRewardStatus := NewSFSObject()
	withRewardStatus.PutUtfString("uid", "reward-1")
	withRewardStatus.PutInt("type", 3)
	withRewardStatus.PutInt("rewardStatus", 0)

	mail := []Mail{{Raw: noRewardStatus}, {Raw: withRewardStatus}}

	if mail[0].HasUnclaimedReward() {
		t.Errorf("HasUnclaimedReward() = true for mail with no rewardStatus field, want false")
	}
	if !mail[1].HasUnclaimedReward() {
		t.Errorf("HasUnclaimedReward() = false for mail with rewardStatus=0, want true")
	}

	got := groupUnclaimedByType(mail)
	if len(got) != 1 {
		t.Fatalf("got %d distinct types, want 1 (the no-rewardStatus mail must be excluded)", len(got))
	}
	if len(got[3]) != 1 || got[3][0] != "reward-1" {
		t.Errorf("type 3: got %v, want [reward-1] -- notif-1 (no rewardStatus) must not appear", got[3])
	}
}

// TestHasUnclaimedRewardWrongTypedRewardStatusIsNotMisclassified is the regression test for
// HasUnclaimedReward's round-29 fix: before it, the guard checked rewardStatus's presence only
// (ok && v.Val != nil), so a PRESENT-BUT-WRONG-TYPED rewardStatus (e.g. sent as a string instead of
// an int) slipped past the guard and then coerced to int32(0) via GetInt's silent zero-value
// coercion -- and the "== 0" comparison deterministically (not just a chance collision)
// misclassified it as unclaimed on every call. Reverting the requireFieldType guard in
// HasUnclaimedReward (back to the old `v, ok := m.Raw.Get("rewardStatus"); !ok || v.Val == nil`
// check) would make this test fail: the wrong-typed mail would then read as unclaimed and get
// bucketed into groupUnclaimedByType's output, ready to be sent in a real mail.reward.batch
// request.
func TestHasUnclaimedRewardWrongTypedRewardStatusIsNotMisclassified(t *testing.T) {
	wrongTyped := NewSFSObject()
	wrongTyped.PutUtfString("uid", "wrong-type-1")
	wrongTyped.PutInt("type", 3)
	wrongTyped.PutUtfString("rewardStatus", "not-an-int") // wrong SFS type: rewardStatus must be an Int

	genuineUnclaimed := NewSFSObject()
	genuineUnclaimed.PutUtfString("uid", "genuine-unclaimed-1")
	genuineUnclaimed.PutInt("type", 3)
	genuineUnclaimed.PutInt("rewardStatus", 0)

	mail := []Mail{{Raw: wrongTyped}, {Raw: genuineUnclaimed}}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	gotWrongTyped := mail[0].HasUnclaimedReward()
	gotGenuine := mail[1].HasUnclaimedReward()
	slog.SetDefault(orig)

	if gotWrongTyped {
		t.Errorf("HasUnclaimedReward() = true for mail with wrong-typed rewardStatus, want false (must not be misclassified as unclaimed)")
	}
	if !gotGenuine {
		t.Errorf("HasUnclaimedReward() = false for mail with rewardStatus=0, want true")
	}

	logged := buf.String()
	if !strings.Contains(logged, "wrong-typed rewardStatus") {
		t.Errorf("expected a wrong-typed-rewardStatus warning, got log:\n%s", logged)
	}

	got := groupUnclaimedByType(mail)
	if len(got) != 1 {
		t.Fatalf("got %d distinct types, want 1 (the wrong-typed-rewardStatus mail must be excluded)", len(got))
	}
	if len(got[3]) != 1 || got[3][0] != "genuine-unclaimed-1" {
		t.Errorf("type 3: got %v, want [genuine-unclaimed-1] -- wrong-type-1 (wrong-typed rewardStatus) must not appear", got[3])
	}
}

// TestGroupUnclaimedByTypeMissingTypeFieldIsExcluded guards the analogous explicit-presence fix in
// groupUnclaimedByType for the `type` field: a reward-bearing mail (HasUnclaimedReward() == true)
// whose `type` key is genuinely absent must be skipped entirely, not defaulted into a `type=0`
// bucket -- GetInt("type") returns the same int32 zero value for "genuinely absent" as it does for
// a real type=0, so without the presence guard this mail would be indistinguishable from, and
// silently merged into, a real type=0 batch. Reverting the requirePresentField guard in
// groupUnclaimedByType (back to bare `byType[m.Type()] = append(...)`) would make this fail, since
// the no-type mail would then be swept into byType[0].
func TestGroupUnclaimedByTypeMissingTypeFieldIsExcluded(t *testing.T) {
	noType := NewSFSObject()
	noType.PutUtfString("uid", "no-type-1")
	noType.PutInt("rewardStatus", 0)
	// deliberately no PutInt("type", ...) call -- field genuinely absent, as opposed to an
	// explicit 0.

	explicitTypeZero := NewSFSObject()
	explicitTypeZero.PutUtfString("uid", "type-zero-1")
	explicitTypeZero.PutInt("type", 0)
	explicitTypeZero.PutInt("rewardStatus", 0)

	mail := []Mail{{Raw: noType}, {Raw: explicitTypeZero}}

	if !mail[0].HasUnclaimedReward() || !mail[1].HasUnclaimedReward() {
		t.Fatalf("both mail entries must be reward-bearing for this test to be meaningful")
	}

	got := groupUnclaimedByType(mail)
	if len(got) != 1 {
		t.Fatalf("got %d distinct types, want 1 (the no-type mail must be excluded, not bucketed under type=0)", len(got))
	}
	if len(got[0]) != 1 || got[0][0] != "type-zero-1" {
		t.Errorf("type 0: got %v, want [type-zero-1] -- no-type-1 (no type field) must not appear", got[0])
	}
}
