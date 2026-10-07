package main

import (
	"strings"
	"testing"
)

// mapsFixture is a minimal lw_season_digging_game.json with the live column positions (id 1,
// type 2, num_width 5, num_height 6, hammer_num 18 as a string) and the known rows.
const mapsFixture = `{"name":"lw_season_digging_game","index":{"id":[1,"number"],"type":[2,"number"],"num_width":[5,"number"],"num_height":[6,"number"],"hammer_num":[18,"string"]},"rows":[
[11001,[11001,5,0,0,6,6,0,0,0,0,0,0,0,0,0,0,0,null]],
[12001,[12001,6,0,0,6,6,0,0,0,0,0,0,0,0,0,0,0,"25"]],
[12501,[12501,7,0,0,4,4,0,0,0,0,0,0,0,0,0,0,0,"16"]],
[13501,[13501,8,0,0,4,4]]]}`

// blocksFixture is a minimal lw_season_block.json (size_width 5, size_height 6).
const blocksFixture = `{"name":"lw_season_block","index":{"id":[1,"number"],"type":[2,"number"],"size_width":[5,"number"],"size_height":[6,"number"]},"rows":[
[10001,[10001,1,0,0,2,2]],[20001,[20001,3,0,0,2,4]],[21001,[21001,4,0,0,1,1]]]}`

func TestParseAndRender(t *testing.T) {
	lvs, err := parseLevels([]byte(mapsFixture))
	if err != nil {
		t.Fatal(err)
	}
	bls, err := parseBlocks([]byte(blocksFixture))
	if err != nil {
		t.Fatal(err)
	}
	code, err := render(lvs, bls, "39432")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"table version 39432", "11001: {w: 6, h: 6, typ: 5},", "12001: {w: 6, h: 6, typ: 6, hammers: 25},",
		"13501: {w: 4, h: 4, typ: 8},", "20001: {w: 2, h: 4},"} {
		if !strings.Contains(string(code), want) {
			t.Errorf("generated code missing %q", want)
		}
	}
}

func TestParseRejectsWrongTable(t *testing.T) {
	if _, err := parseLevels([]byte(strings.Replace(mapsFixture, `"25"]`, `"24"]`, 1))); err == nil {
		t.Error("a known map with the wrong hammer_num must be rejected")
	}
	if _, err := parseLevels([]byte(strings.Replace(mapsFixture, `"name":"lw_season_digging_game"`, `"name":"lw_season_block"`, 1))); err == nil {
		t.Error("another table must be rejected")
	}
	if _, err := parseBlocks([]byte(strings.Replace(blocksFixture, `[20001,3,0,0,2,4]`, `[20001,3,0,0,4,2]`, 1))); err == nil {
		t.Error("a known block with swapped sizes must be rejected")
	}
	if _, err := parseBlocks([]byte(strings.Replace(blocksFixture, `[21001,4,0,0,1,1]`, `[21001,4,0,0,0,1]`, 1))); err == nil {
		t.Error("a zero-size block must be rejected")
	}
}
