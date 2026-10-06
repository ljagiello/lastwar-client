package main

import (
	"strings"
	"testing"
)

// fixture is a minimal score.json: the six known rows plus one extra, with the columns in the
// live table's positions (id 1, type 2, value 4, points 5).
const fixture = `{"name":"score","index":{"id":[1,"number"],"type":[2,"number"],"value":[4,"string"],"points":[5,"number"]},"rows":[
[90402,[90402,82,"","1",10000]],
[90201,[90201,51,"","7",50]],
[90301,[90301,51,"","6",50]],
[90501,[90501,51,"","4",50]],
[90601,[90601,51,"","9",50]],
[90101,[90101,42,"","1",1500]],
[90102,[90102,87,"","1|660",1]]]}`

func TestParseAndRender(t *testing.T) {
	rows, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 || rows[0].id != 90101 {
		t.Fatalf("rows = %+v, want 7 sorted by id", rows)
	}
	code, err := render(rows, "39432")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table version 39432", `90102: {typ: 87, value: "1|660", points: 1}`, `90201: {typ: 51, value: "7", points: 50}`} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q", want)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	shifted := strings.Replace(fixture, `[90201,51,"","7",50]`, `[90201,51,"","8",50]`, 1)
	if _, err := parse([]byte(shifted)); err == nil {
		t.Error("a known row with the wrong value must be rejected")
	}
	if _, err := parse([]byte(strings.Replace(fixture, `"name":"score"`, `"name":"building"`, 1))); err == nil {
		t.Error("another table must be rejected")
	}
}
