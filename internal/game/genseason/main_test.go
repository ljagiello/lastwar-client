package main

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

// dump builds a table dump in the JSON shape of tables40413_json: columns at 1-based positions in
// the order given (linked marks interned columns), each row keyed by its first cell.
func dump(t *testing.T, name string, cols []string, linked map[string]bool, rows ...[]any) []byte {
	t.Helper()
	index := map[string][]any{}
	for i, c := range cols {
		spec := []any{i + 1, "string"}
		if linked[c] {
			spec = append(spec, true)
		}
		index[c] = spec
	}
	var rs [][]any
	for _, r := range rows {
		rs = append(rs, []any{r[0], r})
	}
	b, err := json.Marshal(map[string]any{"name": name, "index": index, "rows": rs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtures are minimal dumps holding the known 40413 rows plus one more each, with cells as the
// dump types them (numbers for number columns, strings for string and linked columns).
func fixtures(t *testing.T) map[string][]byte {
	t.Helper()
	cfgCols := []string{"id", "type", "type1", "season", "trends", "season_achievements", "season_golden_tree"}
	milCols := []string{"id", "group", "level", "unlock_score", "unlock_condition_type", "unlock_condition_para", "standard_level", "need_rank"}
	return map[string][]byte{
		"season": dump(t, "lw_season", cfgCols, nil,
			[]any{3, 1, 0, 1, 10, nil, nil},
			[]any{85, 5, 0, 4, 40, "5000", "10001"},
			[]any{1075, 6, 1, 6, 60, "8000", nil},
			[]any{1477, 4, 1, 7, 70, "9000", nil},
			[]any{1500000, 4, 1, 7, 70, "9000", nil}),
		"achievements": dump(t, "season_achievements", []string{"id", "group", "flag", "not_show"}, nil,
			[]any{7003, 7000, 1, 1}, []any{9001, 9000, 0, 0}, []any{9002, 9000, 1, 0}),
		"military": dump(t, "lw_season_military_level", milCols, nil,
			[]any{102, "60", "2", "12000", nil, nil, nil, nil},
			[]any{701, "70", "1", nil, nil, nil, nil, nil},
			[]any{715, "70", "15", "704420", nil, nil, "1", nil},
			[]any{716, "70", "16", nil, nil, nil, nil, "1"}),
		"trends": dump(t, "lw_season_trends", []string{"id", "season_group", "unlock_time", "para2"}, nil,
			[]any{1, 10, 1, 5}, []any{623, 70, 43, 100000}, []any{624, 70, 50, 10}),
		"activity": dump(t, "activity", []string{"id", "type", "para_2"}, map[string]bool{"para_2": true},
			[]any{1200101, 389, "60"}, []any{1200224, 389, "70"}, []any{1200021, 256, nil}),
		"bloodplan": dump(t, "season_blood_night_plan", []string{"id", "flag"}, nil, []any{1001, 4}, []any{1002, 4}),
		"goldtree":  dump(t, "season_golden_tree", []string{"id", "cycle"}, nil, []any{10001, "2|6"}),
	}
}

func TestParseAndRender(t *testing.T) {
	tabs, err := parseAll(fixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs.configs) != 5 || len(tabs.military) != 4 || len(tabs.militaryActs) != 2 || len(tabs.trends) != 3 {
		t.Fatalf("parsed %+v", tabs)
	}
	code, err := render(tabs, "40413")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(table version 40413)",
		"all 5 rows of table lw_season",
		"3:       {season: 1, typ: 1, trends: 10},",
		"85:      {season: 4, typ: 5, achievements: 5000, trends: 40, goldenTree: 10001},",
		"1477:    {season: 7, typ: 4, type1: 1, achievements: 9000, trends: 70},",
		"{id: 7003, group: 7000, flag: 1, notShow: 1},",
		"{id: 9001, group: 9000},",
		"60: {\n\t\t{level: 2, unlockScore: 12000},\n\t},",
		"{level: 15, unlockScore: 704420, standardLevel: 1},",
		"{level: 16, needRank: 1},",
		"1200224: 70,",
		"623: {group: 70, unlockTime: 43, target: 100000},",
		"1001: 4,",
		`10001: "2|6",`,
	} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
	if strings.Contains(string(code), "1200021") {
		t.Error("only SeasonMilitary (389) rows of table activity belong in seasonMilitaryActivityGroups")
	}
}

func TestParseRejectsWrongTables(t *testing.T) {
	for name, c := range map[string]struct{ table, old, new string }{
		"another table":                {"season", `"name":"lw_season"`, `"name":"lw_season_trends"`},
		"type one cell off":            {"season", `"type":[2,`, `"type":[3,`},
		"a known config changed":       {"season", `[1477,4,1,7,70,"9000",null]`, `[1477,4,1,7,70,"8000",null]`},
		"a key that isn't the id":      {"achievements", `[9002,[9002,`, `[9003,[9002,`},
		"a known threshold changed":    {"military", `"704420"`, `"704421"`},
		"a level twice":                {"military", `"70","16"`, `"70","15"`},
		"military para_2 changed":      {"activity", `[1200224,389,"70"]`, `[1200224,389,"60"]`},
		"a SeasonMilitary without one": {"activity", `[1200101,389,"60"]`, `[1200101,389,null]`},
		"a known trend target changed": {"trends", `[623,70,43,100000]`, `[623,70,43,10000]`},
		"a known golden tree changed":  {"goldtree", `"2|6"`, `"1|6"`},
		"a non-number id":              {"bloodplan", `[1002,[1002,4]]`, `[1002,[1002,"x"]]`},
	} {
		t.Run(name, func(t *testing.T) {
			src := maps.Clone(fixtures(t))
			s := string(src[c.table])
			if !strings.Contains(s, c.old) {
				t.Fatalf("fixture %s lacks %q:\n%s", c.table, c.old, s)
			}
			src[c.table] = []byte(strings.Replace(s, c.old, c.new, 1))
			if _, err := parseAll(src); err == nil {
				t.Error("parseAll accepted it")
			}
		})
	}
}
