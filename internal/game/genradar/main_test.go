package main

import (
	"strings"
	"testing"
)

// eventsFixture is a minimal detect_event.json: the known rows plus a run of three type-6 ids,
// with type in the live table's position (id 1, type 2).
const eventsFixture = `{"name":"detect_event","index":{"id":[1,"number"],"type":[2,"number"]},"rows":[
[100,[100,6]],[101,[101,6]],[102,[102,6]],
[199,[199,11]],[205,[205,2]],[15000,[15000,8]],[20000,[20000,14]],[21001,[21001,16]],
[24000,[24000,18]],[400000,[400000,26]],[410000,[410000,35]],[601001,[601001,44]]]}`

// levelsFixture is a minimal detect_level.json with the live column positions.
const levelsFixture = `{"name":"detect_level","index":{"id":[1,"number"],"exp":[2,"number"],"refresh":[3,"string"],"detect_show_num":[4,"number"],"detect_max_num":[5,"number"]},"rows":[
[1,[1,5,"360;6",5,25]],[16,[16,10000,"360;13",12,40]],[20,[20,25000,"360;13",13,50]]]}`

func TestParseAndRender(t *testing.T) {
	ranges, n, err := parseEvents([]byte(eventsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if n != 12 || ranges[0] != (typeRange{lo: 100, hi: 102, typ: 6}) {
		t.Fatalf("n=%d ranges=%+v, want 12 ids with 100-102 merged", n, ranges)
	}
	lvs, err := parseLevels([]byte(levelsFixture))
	if err != nil {
		t.Fatal(err)
	}
	code, err := render(ranges, n, lvs, "39432")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table version 39432", "{lo: 100, hi: 102, typ: 6}", "{lo: 24000, hi: 24000, typ: 18}",
		"16: {show: 12, max: 40, refreshMin: 360, refreshN: 13}"} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q", want)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	if _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `[24000,[24000,18]]`, `[24000,[24000,2]]`, 1))); err == nil {
		t.Error("a known event with the wrong type must be rejected")
	}
	if _, _, err := parseEvents([]byte(strings.Replace(eventsFixture, `"name":"detect_event"`, `"name":"detect_event_new"`, 1))); err == nil {
		t.Error("another table must be rejected")
	}
	if _, err := parseLevels([]byte(strings.Replace(levelsFixture, `"360;13",12,40`, `"360;13",12,45`, 1))); err == nil {
		t.Error("a known level with the wrong stock cap must be rejected")
	}
	if _, err := parseLevels([]byte(strings.Replace(levelsFixture, `"360;6"`, `"360"`, 1))); err == nil {
		t.Error("a malformed refresh must be rejected")
	}
}
