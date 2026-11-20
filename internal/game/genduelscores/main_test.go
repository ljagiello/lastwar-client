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

// heroFixture is a minimal lw_hero.json with the 40413 column positions (quality 7, max_rank 43,
// max_honorLevel 51, a string column) shrunk to three cells: the known rows plus one extra.
const heroFixture = `{"name":"lw_hero","index":{"id":[1,"number"],"quality":[2,"number"],"max_rank":[3,"number"],"max_honorLevel":[4,"string"]},"rows":[
[50006,[50006,5,26,"600"]],
[10001,[10001,1,26,"600"]],
[550019,[550019,5,26,null]],
[10003,[10003,3,22,null]]]}`

// recruitFixture is a minimal officer_recruit.json; wish_list is a linked column, as in 40413.
const recruitFixture = `{"name":"officer_recruit","index":{"id":[1,"number"],"type":[2,"number"],"time_type":[3,"string"],"wish_list":[4,"string",true]},"rows":[
[401100,[401100,9,null,null]],
[221500,[221500,1,null,null]],
[221501,[221501,1,null,"50006|50008|50009|50016|50015|50017|50019"]],
[301100,[301100,1,"10",null]],
[221708,[221708,1,null,"50006|50014"]]]}`

func TestParseAndRender(t *testing.T) {
	rows, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 || rows[0].id != 90101 {
		t.Fatalf("rows = %+v, want 7 sorted by id", rows)
	}
	heroes, err := parseHeroes([]byte(heroFixture))
	if err != nil {
		t.Fatal(err)
	}
	recruits, err := parseRecruits([]byte(recruitFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(heroes) != 4 || heroes[0].id != 10001 || len(recruits) != 5 || recruits[0].id != 221500 {
		t.Fatalf("heroes %+v, recruits %+v: want every row, sorted by id", heroes, recruits)
	}
	code, err := render(rows, heroes, recruits, "40413")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"table version 40413", `90102: {typ: 87, value: "1|660", points: 1}`, `90201: {typ: 51, value: "7", points: 50}`, "7 rows.",
		"10003:  {quality: 3, maxRank: 22, maxHonorLevel: 0},", "550019: {quality: 5, maxRank: 26, maxHonorLevel: 0},", "4 rows.",
		`"221708": {typ: 1, wishList: []int32{50006, 50014}},`, `"301100": {typ: 1, timeType: "10"},`, `"401100": {typ: 9},`, "5 rows.",
	} {
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
	for name, src := range map[string]string{
		"hero known row":     strings.Replace(heroFixture, `[50006,5,26,"600"]`, `[50006,4,26,"600"]`, 1),
		"hero repeated id":   strings.Replace(heroFixture, `[10003,[10003`, `[10001,[10001`, 1),
		"hero no quality":    strings.Replace(heroFixture, `[10003,3,22,null]`, `[10003,null,22,null]`, 1),
		"hero wrong table":   strings.Replace(heroFixture, `"name":"lw_hero"`, `"name":"score"`, 1),
		"hero linked column": strings.Replace(heroFixture, `"max_rank":[3,"number"]`, `"max_rank":[3,"number",true]`, 1),
	} {
		if _, err := parseHeroes([]byte(src)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	for name, src := range map[string]string{
		"recruit known row":      strings.Replace(recruitFixture, `[401100,9,null,null]`, `[401100,1,null,null]`, 1),
		"recruit wish list":      strings.Replace(recruitFixture, `"50006|50008|50009|50016|50015|50017|50019"`, `"50006|50008"`, 1),
		"recruit unresolved":     strings.Replace(recruitFixture, `[221708,1,null,"50006|50014"]`, `[221708,1,null,17]`, 1),
		"recruit bad hero id":    strings.Replace(recruitFixture, `"50006|50014"`, `"50006|x"`, 1),
		"recruit no wish column": strings.Replace(recruitFixture, `"wish_list"`, `"wishes"`, 1),
	} {
		if _, err := parseRecruits([]byte(src)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}
