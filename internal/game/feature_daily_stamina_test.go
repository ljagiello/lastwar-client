package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// dailyStaminaServer answers the info read with todayFreeStamina (omitted when < 0) and the claim
// with claimReply.
func dailyStaminaServer(t *testing.T, today int32, claimReply *sfs.SFSObject) (*rewardFake, func(*Init) error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == dailyStaminaInfoCmd {
			resp := sfs.NewSFSObject()
			if today >= 0 {
				resp.PutInt("todayFreeStamina", today)
			}
			return resp
		}
		return claimReply
	})
	return fake, func(in *Init) error { return runDailyStamina(conn, in) }
}

func dailyStaminaInit(lastClaim time.Time) *Init {
	return rewardInit(func(raw *sfs.SFSObject) {
		raw.PutLong("lastClaimFreeStaminaTime", lastClaim.UnixMilli())
	})
}

func TestDailyStaminaClaimsWhenCooldownPassed(t *testing.T) {
	fake, run := dailyStaminaServer(t, 1, rewardOK())
	// Last claim 5 h ago in ms: a seconds/ms mix-up would read it as far in the future.
	if err := run(dailyStaminaInit(time.Now().Add(-5 * time.Hour))); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{dailyStaminaInfoCmd, dailyStaminaClaimCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if n := len(fake.requests()[1].Params.Keys()); n != 0 {
		t.Errorf("claim carried %d params, want none", n)
	}
}

func TestDailyStaminaNeverClaimedIsEligible(t *testing.T) {
	fake, run := dailyStaminaServer(t, -1, rewardOK()) // info omits todayFreeStamina: 0
	if err := run(rewardInit(func(*sfs.SFSObject) {})); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); len(got) != 2 || got[1] != dailyStaminaClaimCmd {
		t.Errorf("sent %v, want info then claim", got)
	}
}

func TestDailyStaminaSkipsWhenNotEligible(t *testing.T) {
	for name, c := range map[string]struct {
		today int32
		last  time.Time
	}{
		"cooldown running":  {1, time.Now().Add(-time.Hour)},
		"both claims taken": {2, time.Now().Add(-10 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			fake, run := dailyStaminaServer(t, c.today, rewardOK())
			if err := run(dailyStaminaInit(c.last)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, []string{dailyStaminaInfoCmd}) {
				t.Errorf("sent %v, want only the info read", got)
			}
		})
	}
}

func TestDailyStaminaNoInitSendsNothing(t *testing.T) {
	fake, run := dailyStaminaServer(t, 0, rewardOK())
	if err := run(&Init{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v without an init push", got)
	}
}

func TestDailyStaminaAlreadyClaimedIsBenign(t *testing.T) {
	_, run := dailyStaminaServer(t, 0, rewardErr("120289"))
	if err := run(dailyStaminaInit(time.Now().Add(-5 * time.Hour))); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
}

func TestDailyStaminaClaimFailureIsReturned(t *testing.T) {
	_, run := dailyStaminaServer(t, 0, rewardErr("999999"))
	if err := run(dailyStaminaInit(time.Now().Add(-5 * time.Hour))); err == nil {
		t.Error("run = nil, want the claim failure")
	}
}
