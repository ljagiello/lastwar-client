package main

import (
	"slices"
	"strings"
	"testing"
)

// fixture is a cut-down MailEnum.lua with the real file's layout (MailEnum.lua:1-150, 162-183,
// 287-428): every group, the types Go pins, and a nested table the parser must step over.
const fixture = `MailType = {
  MAIL_SYSTEM = 2,
  MAIL_SYSTEM_EXTRA = 3,
  MAIL_SCOUT_ALERT = 9,
  ARENA_BATTLE_REPORT = 117,
  CANT_CLAIM_ALL = 112,
  TranslationRating = 134,
  Automatic_TranslationRating = 135,
  LW_SEASON_REWARD_MAIL = 143,
  MAIL_WOUNDED = 11
}
MailInternalGroup = {
  MAIL_IN_hide = 1,
  MAIL_IN_report = 2,
  MAIL_IN_alliance = 3,
  MAIL_IN_activity = 4,
  MAIL_IN_system = 5,
  MAIL_IN_daily = 6,
  MAIL_IN_charge = 7,
  MAIL_IN_favor = 8,
  MAIL_IN_season = 9,
  MAIL_IN_MAX = 9
}
MailShowGroup = {
  MailInternalGroup.MAIL_IN_report,
  MailInternalGroup.MAIL_IN_alliance,
  MailInternalGroup.MAIL_IN_activity,
  MailInternalGroup.MAIL_IN_season,
  MailInternalGroup.MAIL_IN_system,
  MailInternalGroup.MAIL_IN_daily,
  MailInternalGroup.MAIL_IN_charge,
  MailInternalGroup.MAIL_IN_favor
}
MailShowNameGroup = {
  [MailInternalGroup.MAIL_IN_season] = {
    title_txt = "100356"
  }
}
MailTypeToInternalGroup = {
  [MailType.LW_SEASON_REWARD_MAIL] = MailInternalGroup.MAIL_IN_season,
  [MailType.MAIL_SCOUT_ALERT] = MailInternalGroup.MAIL_IN_report,
  [MailType.ARENA_BATTLE_REPORT] = MailInternalGroup.MAIL_IN_hide,
  [MailType.MAIL_SYSTEM_EXTRA] = MailInternalGroup.MAIL_IN_alliance,
  [MailType.MAIL_SYSTEM] = MailInternalGroup.MAIL_IN_activity,
  [MailType.CANT_CLAIM_ALL] = MailInternalGroup.MAIL_IN_system,
  [MailType.TranslationRating] = MailInternalGroup.MAIL_IN_system,
  [MailType.Automatic_TranslationRating] = MailInternalGroup.MAIL_IN_system
}
`

func TestParseFixture(t *testing.T) {
	p, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]int64{}
	for _, e := range p.entries {
		got[e.typ] = e.group
	}
	want := map[int64]int64{2: 4, 3: 3, 9: 2, 112: 5, 117: 1, 134: 5, 135: 5, 143: 9}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for typ, g := range want {
		if got[typ] != g {
			t.Errorf("type %d: group %d, want %d", typ, got[typ], g)
		}
	}
	if _, ok := got[11]; ok {
		t.Error("MAIL_WOUNDED has no group in the fixture but got one")
	}
	if !slices.Equal(p.show, []int64{2, 3, 4, 9, 5, 6, 7, 8}) || p.favor != 8 || p.hide != 1 {
		t.Errorf("show %v favor %d hide %d", p.show, p.favor, p.hide)
	}
	code, err := render(p, "1.0.367", "39879")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`mailGroupsVersion      = "1.0.367"`,
		`mailGroupsTableVersion = "39879"`,
		"\t9:   2, // MAIL_SCOUT_ALERT: MAIL_IN_report",
		"var mailShowGroups = []int64{2, 3, 4, 9, 5, 6, 7, 8}",
		"const mailFavorGroup = 8",
	} {
		if !strings.Contains(string(code), s) {
			t.Errorf("rendered code lacks %q:\n%s", s, code)
		}
	}
}

// tableFixture is a mail_group.json with the live table's column layout (id, type, group): it adds
// a type the Lua map lacks, 208 (RECHARGE_REFUND in the live table), moves 9 to another group, and
// repeats 3 with the group the Lua map already gives it.
const tableFixture = `{"name":"mail_group","index":{"group":[3,"number"],"id":[1,"number"],"type":[2,"number"]},` +
	`"rows":[[1,[1,208,5]],[2,[2,9,6]],[3,[3,3,3]]]}`

// The client applies every mail_group row over MailEnum.lua's map (MailDataManager.lua:30-36).
func TestOverlayTheMailGroupTable(t *testing.T) {
	p, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseTable([]byte(tableFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := overlay(&p, rows); err != nil {
		t.Fatal(err)
	}
	got := map[int64]int64{}
	for _, e := range p.entries {
		got[e.typ] = e.group
	}
	for typ, g := range map[int64]int64{208: 5, 9: 6, 3: 3, 2: 4} {
		if got[typ] != g {
			t.Errorf("type %d: group %d, want %d", typ, got[typ], g)
		}
	}
	if len(got) != 9 {
		t.Errorf("%d types, want the fixture's 8 plus 208", len(got))
	}
	code, err := render(p, "1.0.367", "39879")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(code), "208: 5, // type 208: MAIL_IN_system (mail_group)") {
		t.Errorf("rendered code lacks the table's type 208:\n%s", code)
	}

	for name, src := range map[string]string{
		"another table":   strings.Replace(tableFixture, `"mail_group"`, `"mail"`, 1),
		"unknown group":   strings.Replace(tableFixture, "[1,208,5]", "[1,208,12]", 1),
		"type in two":     strings.Replace(tableFixture, "[3,3,3]", "[3,208,6]", 1),
		"no group column": strings.Replace(tableFixture, `"group":`, `"grp":`, 1),
		"string cell":     strings.Replace(tableFixture, "[1,208,5]", `[1,"x",5]`, 1),
	} {
		rows, err := parseTable([]byte(src))
		if err == nil {
			p, _ := parse([]byte(fixture))
			err = overlay(&p, rows)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRejectsChangedEnums(t *testing.T) {
	for name, edit := range map[string][2]string{
		"renumbered group":    {"MAIL_IN_alliance = 3,", "MAIL_IN_alliance = 10,"},
		"renumbered type":     {"CANT_CLAIM_ALL = 112,", "CANT_CLAIM_ALL = 113,"},
		"unknown type":        {"[MailType.MAIL_SYSTEM] =", "[MailType.MAIL_NEW] ="},
		"unknown group":       {"= MailInternalGroup.MAIL_IN_activity,", "= MailInternalGroup.MAIL_IN_new,"},
		"unparsed line":       {"  MAIL_WOUNDED = 11", "  MAIL_WOUNDED = 11 -- note"},
		"duplicate type":      {"[MailType.MAIL_SYSTEM] =", "[MailType.MAIL_SCOUT_ALERT] ="},
		"hide in Claim All":   {"  MailInternalGroup.MAIL_IN_favor\n}", "  MailInternalGroup.MAIL_IN_hide\n}"},
		"missing group table": {"MailShowGroup = {", "MailShownGroups = {"},
	} {
		src := strings.Replace(fixture, edit[0], edit[1], 1)
		if src == fixture {
			t.Fatalf("%s: the edit did not apply", name)
		}
		if _, err := parse([]byte(src)); err == nil {
			t.Errorf("%s: parse accepted it", name)
		}
	}
}
