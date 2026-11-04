package main

import (
	"strings"
	"testing"
)

// tabFixture and detailFixture are minimal dumps with the known rows plus one more each, in the
// 39879 column positions (tab: id 1, max_lv 7 of 12; detail: id 1, science_id 2, science_lv 3,
// value 7 of 17). Cells the generator doesn't read are null.
var (
	tabFixture = `{"name":"lw_alliance_science_tab","index":{"id":[1,"number"],"name":[2,"string"],"max_lv":[7,"number"]},"rows":[` +
		`[10011200,[10011200,"454001",null,null,null,null,10,null,null,null,1,null]],` +
		`[10011100,[10011100,"454009",null,null,null,null,1,null,null,null,1,null]],` +
		`[10011300,[10011300,"454015",null,null,null,null,10,null,null,null,1,null]]]}`
	detailFixture = `{"name":"lw_alliance_science_detail","index":{"id":[1,"number"],"science_id":[2,"number"],"science_lv":[3,"number"],"max_lv":[4,"number"],"value":[7,"number"]},"rows":[` +
		`[10011100,[10011100,10011100,0,1,0,[],10000,7200,2,100,2,2,50,50,50,90015,"0"]],` +
		`[10011101,[10011101,10011100,1,1,0,[],80000,14400,2,200,2,2,50,50,50,90015,"1"]],` +
		`[10011200,[10011200,10011200,0,10,0,["10011101"],100000,7200,2,100,2,2,50,50,50,50129,"0"]],` +
		`[10011210,[10011210,10011200,10,10,0,[],0,7200,2,100,2,2,50,50,50,50129,"0"]]]}`
)

func TestParseAndRender(t *testing.T) {
	lv, err := parseTab([]byte(tabFixture))
	if err != nil {
		t.Fatal(err)
	}
	pro, err := parseDetail([]byte(detailFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(lv) != 3 || lv[0] != (entry{10011100, 1}) || len(pro) != 4 || pro[3] != (entry{10011210, 0}) {
		t.Fatalf("max levels %v, targets %v", lv, pro)
	}
	code, err := render(lv, pro, "39879")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table version 39879",
		`const allianceScienceTableVersion = "39879"`,
		"all 3 rows of table lw_alliance_science_tab",
		"10011100: 1,",
		"10011200: 10,",
		"all 4 rows of table lw_alliance_science_detail",
		"10011101: 80000,",
		"10011210: 0,",
	} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
}

func TestParseRejectsWrongTables(t *testing.T) {
	tab := map[string]string{
		"another table":           strings.Replace(tabFixture, `"name":"lw_alliance_science_tab"`, `"name":"lw_camp_science_tab"`, 1),
		"max_lv one cell off":     strings.Replace(tabFixture, `"max_lv":[7,`, `"max_lv":[6,`, 1),
		"a known max_lv changed":  strings.Replace(tabFixture, `"454009",null,null,null,null,1,`, `"454009",null,null,null,null,2,`, 1),
		"a key that isn't the id": strings.Replace(tabFixture, `[10011300,[10011300,`, `[10011301,[10011300,`, 1),
		"a duplicate id":          strings.Replace(tabFixture, `[10011300,[10011300,`, `[10011200,[10011200,`, 1),
		"no rows":                 `{"name":"lw_alliance_science_tab","index":{"id":[1,"number"],"max_lv":[7,"number"]},"rows":[]}`,
	}
	for name, src := range tab {
		if src == tabFixture {
			t.Errorf("tab %s: the case didn't change the fixture", name)
			continue
		}
		if _, err := parseTab([]byte(src)); err == nil {
			t.Errorf("tab %s must be rejected", name)
		}
	}
	detail := map[string]string{
		"another table":            strings.Replace(detailFixture, `"name":"lw_alliance_science_detail"`, `"name":"lw_camp_science_detail"`, 1),
		"value one cell off":       strings.Replace(detailFixture, `"value":[7,`, `"value":[8,`, 1),
		"a known value changed":    strings.Replace(detailFixture, `[],80000,`, `[],80001,`, 1),
		"id not science_id + lv":   strings.Replace(detailFixture, `[10011210,[10011210,10011200,10,`, `[10011210,[10011210,10011200,9,`, 1),
		"a missing science_lv col": strings.Replace(detailFixture, `"science_lv":[3,"number"],`, ``, 1),
		"a negative value":         strings.Replace(detailFixture, `[],0,7200`, `[],-1,7200`, 1),
	}
	for name, src := range detail {
		if src == detailFixture {
			t.Errorf("detail %s: the case didn't change the fixture", name)
			continue
		}
		if _, err := parseDetail([]byte(src)); err == nil {
			t.Errorf("detail %s must be rejected", name)
		}
	}
}
