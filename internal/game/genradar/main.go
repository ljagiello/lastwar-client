// Command genradar writes internal/game/radar_tables_gen.go from two of the game's data tables:
//
//   - detect_event: every radar event id's detect_event.type (DetectEventType, EnumType.lua:
//     8512-8552), which decides how the client runs an event (a march-free fake march, a talk, a
//     real march or a battle). get.detect.info carries only the eventId; the client looks the type
//     up in this table (DetectEventInfo.lua:84-87). The 6,603 ids compress to a few hundred runs of
//     consecutive ids with one type.
//   - detect_event, again: the claim cap of every treasure event (types 19, 23 and 38), the second
//     field of para2 ("<reward group>;<cap>"). A dug treasure's world point lets that many players
//     claim it (TreasurePointInfo.GetRewardMaxNum, Assembly-CSharp.decompiled.cs:177923-177932).
//   - detect_level: per radar level, the shown slots (detect_show_num), the stock cap
//     (detect_max_num) and the regeneration ("refresh" = "<minutes>;<events>").
//
// Regenerate after a table update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/genradar -events <tables>/detect_event.json -levels <tables>/detect_level.json -version <table version> -out internal/game/radar_tables_gen.go
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
	"maps"
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

// typeRange is a run of consecutive event ids [lo, hi] that share one detect_event.type.
type typeRange struct {
	lo, hi int64
	typ    int64
}

// capRange is a run of consecutive treasure event ids [lo, hi] that share one claim cap.
type capRange struct {
	lo, hi int64
	cap    int64
}

// treasureTypes are the detect_event types whose events become claimable treasure points:
// TREASURE 19, TREASURE_ACTIVITY 23 and OFF_SEASON_TREASURE 38 (EnumType.lua:8512-8552).
var treasureTypes = []int64{19, 23, 38}

type level struct {
	id, show, max, refreshMin, refreshN int64
}

func main() {
	events := flag.String("events", "", "path to the detect_event table's JSON dump (detect_event.json)")
	levels := flag.String("levels", "", "path to the detect_level table's JSON dump (detect_level.json)")
	version := flag.String("version", "", "table version the dumps came from, recorded in the header (e.g. 39432)")
	out := flag.String("out", "radar_tables_gen.go", "output file")
	flag.Parse()
	if *events == "" || *levels == "" || *version == "" {
		log.Fatal("genradar: -events, -levels and -version are required")
	}
	evSrc, err := os.ReadFile(*events)
	if err != nil {
		log.Fatal(err)
	}
	ranges, caps, n, err := parseEvents(evSrc)
	if err != nil {
		log.Fatalf("genradar: %s: %v", *events, err)
	}
	lvSrc, err := os.ReadFile(*levels)
	if err != nil {
		log.Fatal(err)
	}
	lvs, err := parseLevels(lvSrc)
	if err != nil {
		log.Fatalf("genradar: %s: %v", *levels, err)
	}
	code, err := render(ranges, caps, n, lvs, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genradar: wrote %d event ids (%d ranges), %d treasure cap ranges and %d levels to %s", n, len(ranges), len(caps), len(lvs), *out)
}

// knownEventTypes are (event id -> type) facts from DUEL.md §5.2.5 and the 1.0.364 table that the
// dump must reproduce, so a shifted column or another table can't silently turn a march event into
// a march-free one.
var knownEventTypes = map[int64]int64{
	100:    6,  // Geological Sampling (PickGarbage)
	199:    11, // Rescue the Rebellion
	205:    2,  // Kill Zombies
	15000:  8,  // Doom Elite (boss)
	20000:  14, // Rescue the Survivor (plot / talk)
	21001:  16, // Collect Resources (gather march)
	24000:  18, // Help Teammates
	400000: 26, // Dominator treatment
	410000: 35, // cockatrice guide
	601001: 44, // season visitor
}

// knownTreasureCaps are (treasure event id -> claim cap) facts from the 1.0.364 table: the radar
// treasures, 20 claims outside the season-1 ids and 10 in them, and two off-season parties.
var knownTreasureCaps = map[int64]int64{
	25001:   20,
	1025001: 10,
	27015:   50,
	505601:  100,
}

// knownLevels are detect_level rows from DUEL.md §5.2.3.
var knownLevels = []level{
	{id: 1, show: 5, max: 25, refreshMin: 360, refreshN: 6},
	{id: 16, show: 12, max: 40, refreshMin: 360, refreshN: 13},
	{id: 20, show: 13, max: 50, refreshMin: 360, refreshN: 13},
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

func parseEvents(src []byte) ([]typeRange, []capRange, int, error) {
	t, err := decode(src, "detect_event")
	if err != nil {
		return nil, nil, 0, err
	}
	col, err := column(t, "type")
	if err != nil {
		return nil, nil, 0, err
	}
	// para2 is marked linked, but the dump resolves it to the text itself (the table's link is
	// null); parseCap accepts only "<reward group>;<cap>", so an interned index can't pass.
	capCol, err := linkedColumn(t, "para2")
	if err != nil {
		return nil, nil, 0, err
	}
	types := map[int64]int64{}
	caps := map[int64]int64{}
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, nil, 0, err
		}
		typ, ok, err := cell(vals, col)
		if err != nil || !ok || typ <= 0 || typ > math.MaxInt8 {
			return nil, nil, 0, fmt.Errorf("event %d: bad type (%v)", id, err)
		}
		if _, dup := types[id]; dup {
			return nil, nil, 0, fmt.Errorf("event %d: duplicate id", id)
		}
		types[id] = typ
		if !slices.Contains(treasureTypes, typ) {
			continue
		}
		para2, err := text(vals, capCol)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("event %d para2: %v", id, err)
		}
		n, err := parseCap(para2)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("event %d para2 %q: %v", id, para2, err)
		}
		caps[id] = n
	}
	for id, want := range knownEventTypes {
		if got, ok := types[id]; !ok || got != want {
			return nil, nil, 0, fmt.Errorf("event %d type = %d, want %d (wrong table or shifted column)", id, got, want)
		}
	}
	for id, want := range knownTreasureCaps {
		if got, ok := caps[id]; !ok || got != want {
			return nil, nil, 0, fmt.Errorf("treasure event %d cap = %d, want %d (wrong table or shifted column)", id, got, want)
		}
	}
	ids := slices.Sorted(maps.Keys(types))
	var ranges []typeRange
	for _, id := range ids {
		if n := len(ranges); n > 0 && ranges[n-1].hi == id-1 && ranges[n-1].typ == types[id] {
			ranges[n-1].hi = id
			continue
		}
		ranges = append(ranges, typeRange{lo: id, hi: id, typ: types[id]})
	}
	var capRanges []capRange
	for _, id := range slices.Sorted(maps.Keys(caps)) {
		if n := len(capRanges); n > 0 && capRanges[n-1].hi == id-1 && capRanges[n-1].cap == caps[id] {
			capRanges[n-1].hi = id
			continue
		}
		capRanges = append(capRanges, capRange{lo: id, hi: id, cap: caps[id]})
	}
	return ranges, capRanges, len(ids), nil
}

// parseCap reads para2's "<reward group>;<cap>" (GetRewardMaxNum splits on ';' and takes the
// second field, Assembly-CSharp.decompiled.cs:177923-177932).
func parseCap(s string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(s), ";")
	if len(parts) != 2 {
		return 0, errors.New("want <reward group>;<cap>")
	}
	group, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	n, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return 0, err
	}
	if group <= 0 || n <= 0 || n > math.MaxInt16 {
		return 0, errors.New("out-of-range value")
	}
	return n, nil
}

func parseLevels(src []byte) ([]level, error) {
	t, err := decode(src, "detect_level")
	if err != nil {
		return nil, err
	}
	cols := map[string]int{}
	for _, name := range []string{"detect_show_num", "detect_max_num", "refresh"} {
		c, err := column(t, name)
		if err != nil {
			return nil, err
		}
		cols[name] = c
	}
	var lvs []level
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, err
		}
		show, ok1, err1 := cell(vals, cols["detect_show_num"])
		maxNum, ok2, err2 := cell(vals, cols["detect_max_num"])
		if err := errors.Join(err1, err2); err != nil || !ok1 || !ok2 || show <= 0 || maxNum <= 0 {
			return nil, fmt.Errorf("level %d: bad detect_show_num/detect_max_num (%v)", id, err)
		}
		refresh, err := text(vals, cols["refresh"])
		if err != nil {
			return nil, fmt.Errorf("level %d refresh: %v", id, err)
		}
		mins, n, err := parseRefresh(refresh)
		if err != nil {
			return nil, fmt.Errorf("level %d refresh %q: %v", id, refresh, err)
		}
		lvs = append(lvs, level{id: id, show: show, max: maxNum, refreshMin: mins, refreshN: n})
	}
	slices.SortFunc(lvs, func(a, b level) int { return cmp.Compare(a.id, b.id) })
	byID := map[int64]level{}
	for _, l := range lvs {
		byID[l.id] = l
	}
	for _, k := range knownLevels {
		if got, ok := byID[k.id]; !ok || got != k {
			return nil, fmt.Errorf("level %d = %+v, want %+v (wrong table or shifted column)", k.id, got, k)
		}
	}
	return lvs, nil
}

// parseRefresh reads "<minutes>;<events>" (GetEventRecoverNum, UIDetectEventCtrl.lua:286-302).
func parseRefresh(s string) (mins, n int64, err error) {
	parts := strings.Split(strings.TrimSpace(s), ";")
	if len(parts) != 2 {
		return 0, 0, errors.New("want <minutes>;<events>")
	}
	mins, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	n, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return 0, 0, err
	}
	if mins <= 0 || n <= 0 {
		return 0, 0, errors.New("non-positive value")
	}
	return mins, n, nil
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

// linkedColumn is column for a column the index marks linked; the caller must validate every
// cell's format, since a linked cell could be an index into a value table.
func linkedColumn(t table, name string) (int, error) {
	spec, ok := t.Index[name]
	if !ok || len(spec) < 1 {
		return 0, fmt.Errorf("no %s column in index", name)
	}
	pos, ok, err := number(spec[0])
	if err != nil || !ok || pos < 1 {
		return 0, fmt.Errorf("%s: bad column position %s", name, spec[0])
	}
	return int(pos - 1), nil
}

func cell(vals []json.RawMessage, col int) (int64, bool, error) {
	if col >= len(vals) {
		return 0, false, nil
	}
	return number(vals[col])
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

func render(ranges []typeRange, caps []capRange, n int, lvs []level, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genradar from the detect_event and detect_level data tables (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	fmt.Fprintf(&b, "// radarEventTypeRanges maps all %d detect_event ids to detect_event.type (DetectEventType,\n", n)
	b.WriteString("// EnumType.lua:8512-8552) as runs of consecutive ids, sorted by id.\n")
	b.WriteString("var radarEventTypeRanges = []radarTypeRange{\n")
	for _, r := range ranges {
		fmt.Fprintf(&b, "\t{lo: %d, hi: %d, typ: %d},\n", r.lo, r.hi, r.typ)
	}
	b.WriteString("}\n\n")
	b.WriteString("// radarTreasureCaps maps the treasure event ids (detect_event types 19, 23 and 38) to the number of\n")
	b.WriteString("// players who may claim the dug treasure, para2's second field, as runs of consecutive ids, sorted by id.\n")
	b.WriteString("var radarTreasureCaps = []radarCapRange{\n")
	for _, r := range caps {
		fmt.Fprintf(&b, "\t{lo: %d, hi: %d, cap: %d},\n", r.lo, r.hi, r.cap)
	}
	b.WriteString("}\n\n")
	b.WriteString("// radarLevels is detect_level by radar level: shown slots (detect_show_num), stock cap\n")
	b.WriteString("// (detect_max_num) and regeneration (refresh: refreshN events every refreshMin minutes).\n")
	b.WriteString("var radarLevels = map[int64]radarLevelRow{\n")
	for _, l := range lvs {
		fmt.Fprintf(&b, "\t%d: {show: %d, max: %d, refreshMin: %d, refreshN: %d},\n", l.id, l.show, l.max, l.refreshMin, l.refreshN)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
