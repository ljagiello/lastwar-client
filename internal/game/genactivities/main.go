// Command genactivities writes internal/game/activity_tables_gen.go: every row of the game's
// `activity` data table, keyed by activity id, with the row's EnumActivity type and
// needMainCityLevel. Init's `activity` push carries only ids and windows; the client reads the type
// and the HQ gate from this table for every entry (ActivityInfoData.FetchActivityConfigData,
// ActivityInfoData.lua:224-247) and AddOneActivity dispatches on the type
// (ActivityListDataManager.lua:166-923). Every row is emitted, so an id missing from the generated
// table means the tables are stale, not that no feature handles its type. Regenerate after a table
// update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/genactivities -in <tables>/activity.json -version <table version> -out internal/game/activity_tables_gen.go
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

type row struct {
	id     int64
	typ    int64
	needHQ int64
}

func main() {
	in := flag.String("in", "", "path to the activity table's JSON dump (activity.json)")
	version := flag.String("version", "", "table version the dump came from, recorded in the header (e.g. 39879)")
	out := flag.String("out", "activity_tables_gen.go", "output file")
	flag.Parse()
	if *in == "" || *version == "" {
		log.Fatal("genactivities: -in and -version are required")
	}
	src, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := parse(src)
	if err != nil {
		log.Fatalf("genactivities: %s: %v", *in, err)
	}
	code, err := render(rows, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genactivities: wrote %d rows to %s", len(rows), *out)
}

// knownRows are (id, type, needMainCityLevel) facts the client hard-codes in EnumActivity
// (Global/EnumType.lua, Lua 833) that the dump must reproduce, so a shifted type or
// needMainCityLevel column, or another table, can't silently retype every activity.
var knownRows = []row{
	{id: 55000, typ: 14, needHQ: 10},  // AllianceCompete, ActId "55000" (:6266-6270); HQ 10 as in the 1.0.364 table
	{id: 40038, typ: 201, needHQ: 15}, // ActDragon, ActId "40038", needMainCityLevel = 15 (:6344-6348)
	{id: 40039, typ: 218, needHQ: 10}, // ActWinterStorm, ActId "40039", needMainCityLevel = 10 (:6349-6353)
	{id: 40040, typ: 342, needHQ: 15}, // ActMeteorite, ActId "40040" (:6420)
	{id: 7001, typ: 134, needHQ: 0},   // LuckyShop, ActId "7001" (:6340); empty needMainCityLevel
}

func parse(src []byte) ([]row, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var t table
	if err := dec.Decode(&t); err != nil {
		return nil, err
	}
	if t.Name != "activity" {
		return nil, fmt.Errorf("table name %q, want activity", t.Name)
	}
	cols := map[string]int{}
	for _, name := range []string{"id", "type", "needMainCityLevel"} {
		c, err := column(t, name)
		if err != nil {
			return nil, err
		}
		cols[name] = c
	}
	var rows []row
	seen := map[int64]bool{}
	for i, r := range t.Rows {
		if len(r) != 2 {
			return nil, fmt.Errorf("row %d: want [key, values], got %d elements", i, len(r))
		}
		id, ok, err := number(r[0])
		if err != nil || !ok || id <= 0 || id > math.MaxInt32 {
			return nil, fmt.Errorf("row %d: bad key %s", i, r[0])
		}
		if seen[id] {
			return nil, fmt.Errorf("row %d: duplicate id %d", i, id)
		}
		seen[id] = true
		var vals []json.RawMessage
		if err := json.Unmarshal(r[1], &vals); err != nil {
			return nil, fmt.Errorf("row %d: %v", i, err)
		}
		if own, ok, err := cell(vals, cols["id"]); err != nil || !ok || own != id {
			return nil, fmt.Errorf("activity %d: id column is %d (wrong table or shifted column)", id, own)
		}
		// obj.type = tonumber(tabData.type): every row has one, so a missing type is a shifted column.
		typ, ok, err := cell(vals, cols["type"])
		if err != nil || !ok || typ < 0 || typ > math.MaxInt32 {
			return nil, fmt.Errorf("activity %d: bad type %s", id, raw(vals, cols["type"]))
		}
		// obj.needMainCityLevel = tonumber(tabData.needMainCityLevel) or 0: a string column, empty for
		// no gate. Anything but a whole number is rejected rather than guessed.
		lv, err := text(vals, cols["needMainCityLevel"])
		if err != nil {
			return nil, fmt.Errorf("activity %d needMainCityLevel: %v", id, err)
		}
		var needHQ int64
		if lv = strings.TrimSpace(lv); lv != "" {
			needHQ, err = strconv.ParseInt(lv, 10, 32)
			if err != nil || needHQ < 0 {
				return nil, fmt.Errorf("activity %d: bad needMainCityLevel %q", id, lv)
			}
		}
		rows = append(rows, row{id: id, typ: typ, needHQ: needHQ})
	}
	if len(rows) == 0 {
		return nil, errors.New("no rows")
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(a.id, b.id) })
	byID := map[int64]row{}
	for _, r := range rows {
		byID[r.id] = r
	}
	for _, k := range knownRows {
		if got, ok := byID[k.id]; !ok || got != k {
			return nil, fmt.Errorf("activity %d = %+v, want %+v (wrong table or shifted column)", k.id, got, k)
		}
	}
	return rows, nil
}

func column(t table, name string) (int, error) {
	spec, ok := t.Index[name]
	if !ok || len(spec) < 1 {
		return 0, fmt.Errorf("no %s column in index", name)
	}
	pos, ok, err := number(spec[0])
	if err != nil || !ok || pos < 1 {
		return 0, fmt.Errorf("%s: bad column position %s", name, spec[0])
	}
	if len(spec) > 2 && string(spec[2]) == "true" {
		return 0, fmt.Errorf("%s is a linked column", name)
	}
	return int(pos - 1), nil
}

func cell(vals []json.RawMessage, col int) (int64, bool, error) {
	if col >= len(vals) {
		return 0, false, nil
	}
	return number(vals[col])
}

func raw(vals []json.RawMessage, col int) string {
	if col >= len(vals) {
		return "(absent)"
	}
	return string(vals[col])
}

// text reads a cell as a string: JSON strings verbatim, numbers in decimal, null as "".
func text(vals []json.RawMessage, col int) (string, error) {
	if col >= len(vals) {
		return "", nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(vals[col]))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	switch x := v.(type) {
	case nil:
		return "", nil
	case json.Number:
		return x.String(), nil
	case string:
		return x, nil
	default:
		return "", fmt.Errorf("unexpected %T", v)
	}
}

func number(raw json.RawMessage) (int64, bool, error) {
	s, err := text([]json.RawMessage{raw}, 0)
	if err != nil {
		return 0, false, err
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

func render(rows []row, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genactivities from the activity data table (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	b.WriteString("// activityTableVersion is the table version activityTable was generated from.\n")
	fmt.Fprintf(&b, "const activityTableVersion = %s\n\n", strconv.Quote(version))
	fmt.Fprintf(&b, "// activityTable holds all %d rows of table activity, keyed by activity id: typ is the row's\n", len(rows))
	b.WriteString("// EnumActivity type (Global/EnumType.lua:6260-6489) and needHQ its needMainCityLevel, 0 when\n")
	b.WriteString("// empty (ActivityInfoData.lua:228, 247).\n")
	b.WriteString("var activityTable = map[int32]activityRow{\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "\t%d: {typ: %d, needHQ: %d},\n", r.id, r.typ, r.needHQ)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
