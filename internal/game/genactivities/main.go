// Command genactivities writes internal/game/activity_tables_gen.go: every row of the game's
// `activity` data table, keyed by activity id, with the columns the event features read. Init's
// `activity` push carries only ids and windows; the client reads the type, sub-type, HQ gate and
// parameters from this table for every entry (ActivityInfoData.FetchActivityConfigData,
// ActivityInfoData.lua:224-409, 1.0.370) and AddOneActivity dispatches on the type
// (ActivityListDataManager.lua:166-932). Every row is emitted, so an id missing from the generated
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
// row is [key, [values...]] with values in column order. Linked (interned) columns are already
// resolved to their strings by the dump.
type table struct {
	Name  string                       `json:"name"`
	Index map[string][]json.RawMessage `json:"index"`
	Rows  [][]json.RawMessage          `json:"rows"`
}

// row is one activity row. Every field is comparable, so a whole row can be checked against a
// known one.
type row struct {
	id     int64
	typ    int64
	needHQ int64
	// subType is tableInfoType: obj.subType = tonumber(tabData.tableInfoType) (:231), 0 when empty.
	subType int64
	// showUI is showUiType: obj.subViewType = tonumber(tabData.showUiType) or 0 (:232); a name
	// such as "ActivityDailyRank" is 0, as tonumber makes it nil.
	showUI int64
	// groupNumber and groupPriority are checknumber(tabData.group_number / group_priority)
	// (:408-409), 0 when empty.
	groupNumber   int64
	groupPriority int64
	// para, para1 (para_1), condition and conditionGiftbag (condition_giftbag) are the raw cells
	// (:351-358; condition is read per feature, e.g. CrystalBossDataManager.lua:402).
	para             string
	para1            string
	condition        string
	conditionGiftbag string
}

func main() {
	in := flag.String("in", "", "path to the activity table's JSON dump (activity.json)")
	version := flag.String("version", "", "table version the dump came from, recorded in the header (e.g. 40413)")
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
// (Global/EnumType.lua, Lua 835) that the dump must reproduce, so a shifted type or
// needMainCityLevel column, or another table, can't silently retype every activity.
var knownRows = []row{
	{id: 55000, typ: 14, needHQ: 10},  // AllianceCompete, ActId "55000" (:6301-6305); HQ 10 as in the 1.0.364 table
	{id: 40038, typ: 201, needHQ: 15}, // ActDragon, ActId "40038", needMainCityLevel = 15 (:6379-6383)
	{id: 40039, typ: 218, needHQ: 10}, // ActWinterStorm, ActId "40039", needMainCityLevel = 10 (:6384-6388)
	{id: 40040, typ: 342, needHQ: 15}, // ActMeteorite, ActId "40040" (:6455)
	{id: 7001, typ: 134, needHQ: 0},   // LuckyShop, ActId "7001" (:6375); empty needMainCityLevel
}

// knownCell is a value of one of the other columns that Go code relied on before these columns were
// generated (each was hand-copied from the table), so a shifted column, or an interned cell the dump
// left unresolved, fails here instead of feeding a feature a wrong parameter.
type knownCell struct {
	id   int64
	col  string
	want string
}

var knownCells = []knownCell{
	{3009, "tableInfoType", "1"},                      // treasureHuntV1SubTypes (feature_treasure_hunt.go)
	{99162, "tableInfoType", "3"},                     // torchRelayActivityRows (feature_torch_relay.go)
	{13, "showUiType", "2"},                           // battlePassLoginPasses (feature_battle_pass_overflow.go)
	{99044, "showUiType", "9"},                        // battlePassLoginPasses
	{29, "group_number", "100"},                       // armsRaceGroups (feature_arms_race_chests.go)
	{29, "group_priority", "40"},                      // armsRaceGroups
	{1200101, "para_1", "49"},                         // seasonMilitaryNormalDays (feature_season_dailies.go)
	{1000021, "para", "10002"},                        // seasonWeekCards (feature_season_week_card.go)
	{1000021, "condition_giftbag", "2026-7-29|10008"}, // seasonWeekCards
}

// value is the row's cell in column col as the dump spells it; an empty number column is "".
func (r row) value(col string) string {
	n := func(v int64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatInt(v, 10)
	}
	switch col {
	case "tableInfoType":
		return n(r.subType)
	case "showUiType":
		return n(r.showUI)
	case "group_number":
		return n(r.groupNumber)
	case "group_priority":
		return n(r.groupPriority)
	case "para":
		return r.para
	case "para_1":
		return r.para1
	case "condition":
		return r.condition
	case "condition_giftbag":
		return r.conditionGiftbag
	}
	return "(unknown column " + col + ")"
}

// stringCols are the raw string columns. Interned (linked) columns are allowed only here, and their
// cells must be JSON strings or null: a number in one is an index the dump didn't resolve.
var stringCols = []string{"para", "para_1", "condition", "condition_giftbag"}

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
	for _, name := range []string{"id", "type", "needMainCityLevel", "tableInfoType", "showUiType", "group_number", "group_priority"} {
		c, err := column(t, name, false)
		if err != nil {
			return nil, err
		}
		cols[name] = c
	}
	for _, name := range stringCols {
		c, err := column(t, name, true)
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
		out := row{id: id, typ: typ}
		// needMainCityLevel, tableInfoType, group_number and group_priority are string columns of
		// whole numbers, empty for none. Anything else is rejected rather than guessed.
		for _, f := range []struct {
			col string
			dst *int64
		}{
			{"needMainCityLevel", &out.needHQ},
			{"tableInfoType", &out.subType},
			{"group_number", &out.groupNumber},
			{"group_priority", &out.groupPriority},
		} {
			if *f.dst, err = wholeNumber(vals, cols[f.col], false); err != nil {
				return nil, fmt.Errorf("activity %d %s: %v", id, f.col, err)
			}
		}
		// showUiType also holds view names ("ActivityDailyRank"), which tonumber turns into nil and
		// the client into 0.
		if out.showUI, err = wholeNumber(vals, cols["showUiType"], true); err != nil {
			return nil, fmt.Errorf("activity %d showUiType: %v", id, err)
		}
		for _, f := range []struct {
			col string
			dst *string
		}{
			{"para", &out.para},
			{"para_1", &out.para1},
			{"condition", &out.condition},
			{"condition_giftbag", &out.conditionGiftbag},
		} {
			if *f.dst, err = stringCell(vals, cols[f.col]); err != nil {
				return nil, fmt.Errorf("activity %d %s: %v", id, f.col, err)
			}
		}
		rows = append(rows, out)
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
		got, ok := byID[k.id]
		if !ok || got.typ != k.typ || got.needHQ != k.needHQ {
			return nil, fmt.Errorf("activity %d = {typ: %d, needHQ: %d}, want {typ: %d, needHQ: %d} (wrong table or shifted column)",
				k.id, got.typ, got.needHQ, k.typ, k.needHQ)
		}
	}
	for _, k := range knownCells {
		got, ok := byID[k.id]
		if !ok || got.value(k.col) != k.want {
			return nil, fmt.Errorf("activity %d %s = %q, want %q (shifted column or unresolved cell)", k.id, k.col, got.value(k.col), k.want)
		}
	}
	return rows, nil
}

// column returns the 0-based position of the named column. A linked (interned) column is refused
// unless linkedOK: the number columns must hold their values directly.
func column(t table, name string, linkedOK bool) (int, error) {
	spec, ok := t.Index[name]
	if !ok || len(spec) < 1 {
		return 0, fmt.Errorf("no %s column in index", name)
	}
	pos, ok, err := number(spec[0])
	if err != nil || !ok || pos < 1 {
		return 0, fmt.Errorf("%s: bad column position %s", name, spec[0])
	}
	if !linkedOK && len(spec) > 2 && string(spec[2]) == "true" {
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

// wholeNumber reads a string cell holding a whole number, 0 when empty. With nameOK a cell that is
// no number at all (tonumber gives nil) reads as 0; a fraction is always an error.
func wholeNumber(vals []json.RawMessage, col int, nameOK bool) (int64, error) {
	s, err := text(vals, col)
	if err != nil {
		return 0, err
	}
	if s = strings.TrimSpace(s); s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err == nil && n >= 0 {
		return n, nil
	}
	if _, ferr := strconv.ParseFloat(s, 64); nameOK && ferr != nil {
		return 0, nil
	}
	return 0, fmt.Errorf("bad value %q", s)
}

// stringCell reads a raw string cell: a JSON string verbatim, null or absent as "". A number is an
// error, since in an interned column it is an index the dump didn't resolve.
func stringCell(vals []json.RawMessage, col int) (string, error) {
	if col >= len(vals) {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(vals[col], &v); err != nil {
		return "", err
	}
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	default:
		return "", fmt.Errorf("want a string, got %s", vals[col])
	}
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

// literal is a row's Go composite literal: typ always, every other field only when set.
func (r row) literal() string {
	parts := []string{fmt.Sprintf("typ: %d", r.typ)}
	for _, f := range []struct {
		name string
		v    int64
	}{{"needHQ", r.needHQ}, {"subType", r.subType}, {"showUIType", r.showUI}, {"groupNumber", r.groupNumber}, {"groupPriority", r.groupPriority}} {
		if f.v != 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", f.name, f.v))
		}
	}
	for _, f := range []struct{ name, v string }{
		{"para", r.para}, {"para1", r.para1}, {"condition", r.condition}, {"conditionGiftbag", r.conditionGiftbag},
	} {
		if f.v != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", f.name, strconv.Quote(f.v)))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func render(rows []row, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genactivities from the activity data table (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	b.WriteString("// activityTableVersion is the table version activityTable was generated from.\n")
	fmt.Fprintf(&b, "const activityTableVersion = %s\n\n", strconv.Quote(version))
	fmt.Fprintf(&b, "// activityTable holds all %d rows of table activity, keyed by activity id, with the columns\n", len(rows))
	b.WriteString("// activityRow describes (ActivityInfoData.FetchActivityConfigData, ActivityInfoData.lua:224-409).\n")
	b.WriteString("var activityTable = map[int32]activityRow{\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "\t%d: %s,\n", r.id, r.literal())
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
