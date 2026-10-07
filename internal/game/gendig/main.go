// Command gendig writes internal/game/dig_tables_gen.go from two of the game's data tables:
//
//   - lw_season_digging_game: every dig map config (mapConfigId) with its grid (num_width x
//     num_height), its game type and hammer_num, the hammers the map's free grant gives where the
//     table states it. The client reads it in DiggingDataTemplateManager:GetConfigData
//     (DiggingDataTemplateManager.lua:12-28): bricks are numbered 1..w*h row by row
//     (DiggingDataManager.lua:86-93), so the grid is needed to know which bricks a block covers.
//   - lw_season_block: every block (gem) id with its size in bricks (size_width x size_height),
//     read by GetConfigDataBlock (:29-45) and DiggingDataManager:CheckBlockGet (:134-148).
//
// Regenerate after a table update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/gendig -maps <tables>/lw_season_digging_game.json -blocks <tables>/lw_season_block.json -version <table version> -out internal/game/dig_tables_gen.go
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"log"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

// table is the JSON dump's shape: index maps column name -> [position, type, linked?], and each
// row is [key, [values...]] with values in column order.
type table struct {
	Name  string                       `json:"name"`
	Index map[string][]json.RawMessage `json:"index"`
	Rows  [][]json.RawMessage          `json:"rows"`
}

type level struct {
	id, w, h, typ, hammers int64
}

type block struct {
	id, w, h int64
}

func main() {
	maps := flag.String("maps", "", "path to the lw_season_digging_game table's JSON dump")
	blocks := flag.String("blocks", "", "path to the lw_season_block table's JSON dump")
	version := flag.String("version", "", "table version the dumps came from, recorded in the header (e.g. 39432)")
	out := flag.String("out", "dig_tables_gen.go", "output file")
	flag.Parse()
	if *maps == "" || *blocks == "" || *version == "" {
		log.Fatal("gendig: -maps, -blocks and -version are required")
	}
	mapSrc, err := os.ReadFile(*maps)
	if err != nil {
		log.Fatal(err)
	}
	lvs, err := parseLevels(mapSrc)
	if err != nil {
		log.Fatalf("gendig: %s: %v", *maps, err)
	}
	blockSrc, err := os.ReadFile(*blocks)
	if err != nil {
		log.Fatal(err)
	}
	bls, err := parseBlocks(blockSrc)
	if err != nil {
		log.Fatalf("gendig: %s: %v", *blocks, err)
	}
	code, err := render(lvs, bls, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("gendig: wrote %d maps and %d blocks to %s", len(lvs), len(bls), *out)
}

// knownLevels are rows of the 1.0.364 table the dump must reproduce, one per game the feature
// plays: 11001 secret vault stage 1 (type 5), 12001 its timed stage (type 6, 25 hammers), 12501 a
// radar ruin (type 7, 16 hammers for 16 bricks), 13501 the city ruin's first stage (type 8).
var knownLevels = []level{
	{id: 11001, w: 6, h: 6, typ: 5},
	{id: 12001, w: 6, h: 6, typ: 6, hammers: 25},
	{id: 12501, w: 4, h: 4, typ: 7, hammers: 16},
	{id: 13501, w: 4, h: 4, typ: 8},
}

// knownBlocks are lw_season_block rows the dump must reproduce.
var knownBlocks = []block{
	{id: 10001, w: 2, h: 2},
	{id: 20001, w: 2, h: 4},
	{id: 21001, w: 1, h: 1},
}

func decode(src []byte, name string) (table, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var t table
	if err := dec.Decode(&t); err != nil {
		return t, err
	}
	if t.Name != name {
		return t, fmt.Errorf("table name %q, want %s", t.Name, name)
	}
	if len(t.Rows) == 0 {
		return t, errors.New("no rows")
	}
	return t, nil
}

// rowValues splits one [key, [values...]] row.
func rowValues(i int, r []json.RawMessage) (int64, []json.RawMessage, error) {
	if len(r) != 2 {
		return 0, nil, fmt.Errorf("row %d: want [key, values], got %d elements", i, len(r))
	}
	id, ok, err := number(r[0])
	if err != nil || !ok || id <= 0 || id > math.MaxInt32 {
		return 0, nil, fmt.Errorf("row %d: bad key %s", i, r[0])
	}
	var vals []json.RawMessage
	if err := json.Unmarshal(r[1], &vals); err != nil {
		return 0, nil, fmt.Errorf("row %d: %v", i, err)
	}
	return id, vals, nil
}

func columns(t table, names ...string) (map[string]int, error) {
	cols := map[string]int{}
	for _, name := range names {
		spec, ok := t.Index[name]
		if !ok || len(spec) < 1 {
			return nil, fmt.Errorf("no %s column in index", name)
		}
		pos, ok, err := number(spec[0])
		if err != nil || !ok || pos < 1 {
			return nil, fmt.Errorf("%s: bad column position %s", name, spec[0])
		}
		if len(spec) > 2 && string(spec[2]) == "true" {
			return nil, fmt.Errorf("%s is a linked column", name)
		}
		cols[name] = int(pos - 1)
	}
	return cols, nil
}

// cell reads a numeric cell; a string holding a decimal counts (hammer_num is a string column).
func cell(vals []json.RawMessage, col int) (int64, bool, error) {
	if col >= len(vals) {
		return 0, false, nil
	}
	return number(vals[col])
}

func number(raw json.RawMessage) (int64, bool, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return 0, false, err
	}
	var s string
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	default:
		return 0, false, fmt.Errorf("unexpected %T", v)
	}
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func parseLevels(src []byte) ([]level, error) {
	t, err := decode(src, "lw_season_digging_game")
	if err != nil {
		return nil, err
	}
	cols, err := columns(t, "num_width", "num_height", "type", "hammer_num")
	if err != nil {
		return nil, err
	}
	var lvs []level
	seen := map[int64]bool{}
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, err
		}
		w, ok1, err1 := cell(vals, cols["num_width"])
		h, ok2, err2 := cell(vals, cols["num_height"])
		typ, ok3, err3 := cell(vals, cols["type"])
		hammers, _, err4 := cell(vals, cols["hammer_num"])
		if err := errors.Join(err1, err2, err3, err4); err != nil || !ok1 || !ok2 || !ok3 || w <= 0 || h <= 0 || w*h > 1000 || hammers < 0 {
			return nil, fmt.Errorf("map %d: bad num_width/num_height/type/hammer_num (%v)", id, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("map %d: duplicate id", id)
		}
		seen[id] = true
		lvs = append(lvs, level{id: id, w: w, h: h, typ: typ, hammers: hammers})
	}
	slices.SortFunc(lvs, func(a, b level) int { return cmp.Compare(a.id, b.id) })
	for _, k := range knownLevels {
		if i := slices.IndexFunc(lvs, func(l level) bool { return l.id == k.id }); i < 0 || lvs[i] != k {
			return nil, fmt.Errorf("map %d: want %+v (missing row, wrong table or shifted column)", k.id, k)
		}
	}
	return lvs, nil
}

func parseBlocks(src []byte) ([]block, error) {
	t, err := decode(src, "lw_season_block")
	if err != nil {
		return nil, err
	}
	cols, err := columns(t, "size_width", "size_height")
	if err != nil {
		return nil, err
	}
	var bls []block
	seen := map[int64]bool{}
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, err
		}
		w, ok1, err1 := cell(vals, cols["size_width"])
		h, ok2, err2 := cell(vals, cols["size_height"])
		if err := errors.Join(err1, err2); err != nil || !ok1 || !ok2 || w <= 0 || h <= 0 || w > 32 || h > 32 {
			return nil, fmt.Errorf("block %d: bad size_width/size_height (%v)", id, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("block %d: duplicate id", id)
		}
		seen[id] = true
		bls = append(bls, block{id: id, w: w, h: h})
	}
	slices.SortFunc(bls, func(a, b block) int { return cmp.Compare(a.id, b.id) })
	for _, k := range knownBlocks {
		if i := slices.IndexFunc(bls, func(b block) bool { return b.id == k.id }); i < 0 || bls[i] != k {
			return nil, fmt.Errorf("block %d: want %+v (missing row, wrong table or shifted column)", k.id, k)
		}
	}
	return bls, nil
}

func render(lvs []level, bls []block, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by gendig from the lw_season_digging_game and lw_season_block data tables (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	fmt.Fprintf(&b, "// digLevels is all %d lw_season_digging_game rows by mapConfigId: the brick grid (num_width x\n", len(lvs))
	b.WriteString("// num_height), the game type and hammer_num (0 where the table leaves it empty).\n")
	b.WriteString("var digLevels = map[int32]digLevel{\n")
	for _, l := range lvs {
		if l.hammers > 0 {
			fmt.Fprintf(&b, "\t%d: {w: %d, h: %d, typ: %d, hammers: %d},\n", l.id, l.w, l.h, l.typ, l.hammers)
		} else {
			fmt.Fprintf(&b, "\t%d: {w: %d, h: %d, typ: %d},\n", l.id, l.w, l.h, l.typ)
		}
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "// digBlockSizes is all %d lw_season_block rows by block id: the bricks a block covers\n", len(bls))
	b.WriteString("// (size_width x size_height) from its top-left brick.\n")
	b.WriteString("var digBlockSizes = map[int32]digBlockSize{\n")
	for _, bl := range bls {
		fmt.Fprintf(&b, "\t%d: {w: %d, h: %d},\n", bl.id, bl.w, bl.h)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
