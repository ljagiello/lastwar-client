package main

import (
	"strconv"
	"strings"
	"testing"
)

// fixtureIndex is the 40413 activity index for the columns the generator reads (positions 1-based,
// 137 columns; para and para_1 are interned, the dump resolves them).
const fixtureIndex = `"id":[1,"number"],"name":[2,"string"],"showUiType":[10,"string"],"type":[15,"number"],` +
	`"condition":[19,"string"],"tableInfoType":[33,"string"],"para":[70,"string",true],"para_1":[71,"string",true],` +
	`"needMainCityLevel":[81,"string"],"group_number":[125,"string"],"group_priority":[126,"string"],` +
	`"condition_giftbag":[135,"string"]`

// fixtureRow is one row of the fixture: cells holds raw JSON by 1-based column position; every
// other cell is null.
type fixtureRow struct {
	id    string
	cells map[int]string
}

// fixtureRows are the five known rows, 99202 (the 2026 Halloween sign-in, type 158, no HQ gate),
// 99301 (the 2026 Thanksgiving sign-in, new in 40413), 11 (type 105, needMainCityLevel "0") and the
// rows whose other columns knownCells pins, with their 40413 values.
var fixtureRows = []fixtureRow{
	{"11", map[int]string{15: "105", 81: `"0"`}},
	{"13", map[int]string{15: "27", 10: `"2"`, 33: `"2"`, 70: `"7610001;7610002;7610003;7610004;7610005;7610006;7610007"`, 19: `"1|4"`}},
	{"29", map[int]string{15: "125", 33: `"4"`, 70: `"1"`, 19: `"1|7"`, 125: `"100"`, 126: `"40"`}},
	{"3009", map[int]string{15: "130", 33: `"1"`, 19: `"1|1"`}},
	{"7001", map[int]string{15: "134", 33: `"1"`, 19: `"1|1"`}},
	{"40038", map[int]string{15: "201", 81: `"15"`}},
	{"40039", map[int]string{15: "218", 81: `"10"`}},
	{"40040", map[int]string{15: "342", 81: `"15"`}},
	{"55000", map[int]string{15: "14", 81: `"10"`, 19: `"1|10"`}},
	{"99044", map[int]string{15: "27", 10: `"9"`, 33: `"2"`, 19: `"1|4"`}},
	{"99162", map[int]string{15: "309", 33: `"3"`, 19: `"1|4"`}},
	{"99202", map[int]string{15: "158", 10: `"2"`, 70: `"1;279113750|2;279113751"`, 19: `"1|4"`}},
	{"99301", map[int]string{15: "158", 10: `"2"`, 70: `"1;280000206|2;280000207"`, 19: `"1|4"`}},
	{"1000021", map[int]string{15: "228", 10: `"7"`, 70: `"10002"`, 71: `"640063|640062|640061"`, 19: `"1|4"`, 81: `"4"`, 135: `"2026-7-29|10008"`}},
	{"1200101", map[int]string{15: "389", 70: `"650053"`, 71: `"49"`, 19: `"1|10"`}},
	{"1200102", map[int]string{15: "105", 10: `"ActivityDailyRank"`}},
}

// mkFixture renders rows as an activity.json dump.
func mkFixture(rows []fixtureRow) string {
	var out []string
	for _, r := range rows {
		vals := make([]string, 137)
		for i := range vals {
			vals[i] = "null"
		}
		vals[0], vals[1] = r.id, `"name_`+r.id+`"`
		for pos, v := range r.cells {
			vals[pos-1] = v
		}
		out = append(out, "["+r.id+",["+strings.Join(vals, ",")+"]]")
	}
	return `{"name":"activity","index":{` + fixtureIndex + `},"rows":[` + strings.Join(out, ",\n") + `]}`
}

var fixture = mkFixture(fixtureRows)

// withCell returns the fixture with one cell of one row replaced (raw JSON, "" meaning null).
func withCell(id string, pos int, v string) string {
	rows := make([]fixtureRow, len(fixtureRows))
	for i, r := range fixtureRows {
		cells := map[int]string{}
		for k, c := range r.cells {
			cells[k] = c
		}
		if r.id == id {
			if v == "" {
				delete(cells, pos)
			} else {
				cells[pos] = v
			}
		}
		rows[i] = fixtureRow{r.id, cells}
	}
	return mkFixture(rows)
}

func TestParseAndRender(t *testing.T) {
	rows, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(fixtureRows) || rows[0] != (row{id: 11, typ: 105}) {
		t.Fatalf("rows = %+v, want all %d sorted by id", rows, len(fixtureRows))
	}
	byID := map[int64]row{}
	for _, r := range rows {
		byID[r.id] = r
	}
	wantRows := map[int64]row{
		99301:   {id: 99301, typ: 158, showUI: 2, para: "1;280000206|2;280000207", condition: "1|4"},
		29:      {id: 29, typ: 125, subType: 4, para: "1", condition: "1|7", groupNumber: 100, groupPriority: 40},
		1000021: {id: 1000021, typ: 228, needHQ: 4, showUI: 7, para: "10002", para1: "640063|640062|640061", condition: "1|4", conditionGiftbag: "2026-7-29|10008"},
		1200102: {id: 1200102, typ: 105}, // showUiType "ActivityDailyRank": tonumber gives nil, the client 0
	}
	for id, want := range wantRows {
		if byID[id] != want {
			t.Errorf("row %d = %+v, want %+v", id, byID[id], want)
		}
	}
	code, err := render(rows, "40413")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table version 40413",
		`const activityTableVersion = "40413"`,
		"all " + strconv.Itoa(len(fixtureRows)) + " rows",
		"11:      {typ: 105},",
		"55000:   {typ: 14, needHQ: 10, condition: \"1|10\"},",
		"99202:   {typ: 158, showUIType: 2, para: \"1;279113750|2;279113751\", condition: \"1|4\"},",
		"99301:   {typ: 158, showUIType: 2, para: \"1;280000206|2;280000207\", condition: \"1|4\"},",
		"29:      {typ: 125, subType: 4, groupNumber: 100, groupPriority: 40, para: \"1\", condition: \"1|7\"},",
		"1000021: {typ: 228, needHQ: 4, showUIType: 7, para: \"10002\", para1: \"640063|640062|640061\", condition: \"1|4\", conditionGiftbag: \"2026-7-29|10008\"},",
		"1200101: {typ: 389, para: \"650053\", para1: \"49\", condition: \"1|10\"},",
	} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	cases := map[string]string{
		"another table":                       strings.Replace(fixture, `"name":"activity"`, `"name":"activity_stage"`, 1),
		"a known row with the wrong type":     withCell("40039", 15, "219"),
		"a known row with the wrong level":    withCell("40039", 81, `"11"`),
		"a type column one cell off":          strings.Replace(fixture, `"type":[15,`, `"type":[14,`, 1),
		"a level column one cell off":         strings.Replace(fixture, `"needMainCityLevel":[81,`, `"needMainCityLevel":[80,`, 1),
		"a missing type column":               strings.Replace(fixture, `"type":[15,"number"],`, ``, 1),
		"a missing para_1 column":             strings.Replace(fixture, `"para_1":[71,"string",true],`, ``, 1),
		"a linked type column":                strings.Replace(fixture, `"type":[15,"number"]`, `"type":[15,"number",true]`, 1),
		"a non-numeric level":                 withCell("11", 81, `"x"`),
		"a fractional level":                  withCell("11", 81, `"1.5"`),
		"a non-numeric tableInfoType":         withCell("11", 33, `"x"`),
		"a fractional showUiType":             withCell("11", 10, `"1.5"`),
		"a negative group_number":             withCell("11", 125, `"-1"`),
		"a null type":                         withCell("99202", 15, ""),
		"an unresolved interned para":         withCell("11", 70, "17"),
		"a known tableInfoType changed":       withCell("3009", 33, `"2"`),
		"a known showUiType changed":          withCell("13", 10, `"1"`),
		"a known group changed":               withCell("29", 126, `"10"`),
		"a known para_1 changed":              withCell("1200101", 71, `"48"`),
		"a known para column one cell off":    strings.Replace(fixture, `"para":[70,`, `"para":[71,`, 1),
		"a known condition_giftbag dropped":   withCell("1000021", 135, ""),
		"a tableInfoType column one cell off": strings.Replace(fixture, `"tableInfoType":[33,`, `"tableInfoType":[32,`, 1),
		"a key that isn't the row's id":       strings.Replace(fixture, `[11,[11,`, `[12,[11,`, 1),
		"a duplicate id":                      strings.Replace(fixture, `[11,[11,`, `[7001,[7001,`, 1),
		"no rows":                             `{"name":"activity","index":{` + fixtureIndex + `},"rows":[]}`,
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
