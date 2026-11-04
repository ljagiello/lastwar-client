package main

import (
	"strings"
	"testing"
)

// fixture is a minimal activity.json: the five known rows plus 99202 (the 2026 Halloween sign-in,
// type 158, no HQ gate) and 11 (type 105, needMainCityLevel "0"), with id, type and
// needMainCityLevel in the live table's positions (1, 15 and 81 of 137). Cells the generator
// doesn't read are null.
var fixture = func() string {
	mk := func(id, typ, needHQ string) string {
		vals := make([]string, 137)
		for i := range vals {
			vals[i] = "null"
		}
		vals[0], vals[1], vals[14], vals[80] = id, `"name_`+id+`"`, typ, needHQ
		return "[" + id + ",[" + strings.Join(vals, ",") + "]]"
	}
	rows := []string{
		mk("11", "105", `"0"`),
		mk("7001", "134", "null"),
		mk("40038", "201", `"15"`),
		mk("40039", "218", `"10"`),
		mk("40040", "342", `"15"`),
		mk("55000", "14", `"10"`),
		mk("99202", "158", "null"),
	}
	return `{"name":"activity","index":{"id":[1,"number"],"name":[2,"string"],"type":[15,"number"],"needMainCityLevel":[81,"string"]},"rows":[` +
		strings.Join(rows, ",\n") + `]}`
}()

func TestParseAndRender(t *testing.T) {
	rows, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 || rows[0] != (row{id: 11, typ: 105}) || rows[6] != (row{id: 99202, typ: 158}) {
		t.Fatalf("rows = %+v, want all 7 sorted by id", rows)
	}
	code, err := render(rows, "39879")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table version 39879",
		`const activityTableVersion = "39879"`,
		"all 7 rows",
		"11:    {typ: 105, needHQ: 0},",
		"55000: {typ: 14, needHQ: 10},",
		"99202: {typ: 158, needHQ: 0},",
	} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	// typeCell is the prefix of a row up to its type cell (values 3-14 are null in the fixture).
	typeCell := func(id string) string { return `"name_` + id + `",` + strings.Repeat("null,", 12) }
	cases := map[string]string{
		"another table":                    strings.Replace(fixture, `"name":"activity"`, `"name":"activity_stage"`, 1),
		"a known row with the wrong type":  strings.Replace(fixture, typeCell("40039")+"218", typeCell("40039")+"219", 1),
		"a known row with the wrong level": strings.Replace(fixture, `"10",null`, `"11",null`, 1),
		"a type column one cell off":       strings.Replace(fixture, `"type":[15,`, `"type":[14,`, 1),
		"a level column one cell off":      strings.Replace(fixture, `"needMainCityLevel":[81,`, `"needMainCityLevel":[80,`, 1),
		"a missing type column":            strings.Replace(fixture, `"type":[15,"number"],`, ``, 1),
		"a non-numeric level":              strings.Replace(fixture, `"0",null`, `"x",null`, 1),
		"a fractional level":               strings.Replace(fixture, `"0",null`, `"1.5",null`, 1),
		"a null type":                      strings.Replace(fixture, typeCell("99202")+"158", typeCell("99202")+"null", 1),
		"a key that isn't the row's id":    strings.Replace(fixture, `[11,[11,`, `[12,[11,`, 1),
		"a duplicate id":                   strings.Replace(fixture, `[11,[11,`, `[7001,[7001,`, 1),
		"no rows":                          `{"name":"activity","index":{"id":[1,"number"],"type":[15,"number"],"needMainCityLevel":[81,"string"]},"rows":[]}`,
	}
	for name, src := range cases {
		if src == fixture {
			t.Errorf("%s: the case didn't change the fixture", name)
			continue
		}
		if _, err := parse([]byte(src)); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}
