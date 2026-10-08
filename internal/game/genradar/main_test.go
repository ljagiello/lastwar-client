package main

import (
	"slices"
	"strings"
	"testing"
)

// eventsFixture is a minimal detect_event.json: the known rows plus a run of three type-6 ids, and
// the known treasure rows plus 25002 (same cap as 25001) and 25003 (another cap), with type and
// the linked para2 in the live table's positions (id 1, type 2, para2 17).
const eventsFixture = `{"name":"detect_event","index":{"id":[1,"number"],"type":[2,"number"],"para2":[17,"string",true]},"link":null,"rows":[
[100,[100,6]],[101,[101,6]],[102,[102,6]],
[199,[199,11]],[205,[205,2]],[15000,[15000,8]],[20000,[20000,14]],[21001,[21001,16]],
[24000,[24000,18,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"1;1"]],
[400000,[400000,26]],[410000,[410000,35]],[601001,[601001,44]],
[25001,[25001,19,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"105123001;20"]],
[25002,[25002,19,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"105123002;20"]],
[25003,[25003,19,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"105123003;15"]],
[27015,[27015,38,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"104120118;50"]],
[505601,[505601,38,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"109000065;100"]],
[1025001,[1025001,19,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"105123001;10"]]]}`

// levelsFixture is a minimal detect_level.json with the live column positions.
const levelsFixture = `{"name":"detect_level","index":{"id":[1,"number"],"exp":[2,"number"],"refresh":[3,"string"],"detect_show_num":[4,"number"],"detect_max_num":[5,"number"]},"rows":[
[1,[1,5,"360;6",5,25]],[16,[16,10000,"360;13",12,40]],[20,[20,25000,"360;13",13,50]]]}`

func TestParseAndRender(t *testing.T) {
	ranges, caps, n, err := parseEvents([]byte(eventsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if n != 18 || ranges[0] != (typeRange{lo: 100, hi: 102, typ: 6}) {
		t.Fatalf("n=%d ranges=%+v, want 18 ids with 100-102 merged", n, ranges)
	}
	wantCaps := []capRange{{25001, 25002, 20}, {25003, 25003, 15}, {27015, 27015, 50}, {505601, 505601, 100}, {1025001, 1025001, 10}}
	if !slices.Equal(caps, wantCaps) {
		t.Fatalf("caps = %+v, want %+v (the type-18 row's para2 is not a treasure cap)", caps, wantCaps)
	}
	lvs, err := parseLevels([]byte(levelsFixture))
	if err != nil {
		t.Fatal(err)
	}
	code, err := render(ranges, caps, n, lvs, "39432")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table version 39432", "{lo: 100, hi: 102, typ: 6}", "{lo: 24000, hi: 24000, typ: 18}",
		"{lo: 25001, hi: 25002, cap: 20}", "16: {show: 12, max: 40, refreshMin: 360, refreshN: 13}"} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q", want)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	if _, _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `[24000,[24000,18,`, `[24000,[24000,2,`, 1))); err == nil {
		t.Error("a known event with the wrong type must be rejected")
	}
	if _, _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `"name":"detect_event"`, `"name":"detect_event_new"`, 1))); err == nil {
		t.Error("another table must be rejected")
	}
	if _, _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `"105123001;10"`, `"105123001;20"`, 1))); err == nil {
		t.Error("a known treasure with the wrong cap must be rejected")
	}
	for _, bad := range []string{`"7"`, `7`, `"105123002;"`, `"105123002;0"`, `"a;20"`, `null`} {
		if _, _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `"105123002;20"`, bad, 1))); err == nil {
			t.Errorf("treasure para2 %s must be rejected (an interned index or a malformed cap)", bad)
		}
	}
	if _, err := parseLevels([]byte(strings.Replace(levelsFixture, `"360;13",12,40`, `"360;13",12,45`, 1))); err == nil {
		t.Error("a known level with the wrong stock cap must be rejected")
	}
	if _, err := parseLevels([]byte(strings.Replace(levelsFixture, `"360;6"`, `"360"`, 1))); err == nil {
		t.Error("a malformed refresh must be rejected")
	}
}
