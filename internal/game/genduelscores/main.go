// Command genduelscores writes internal/game/duel_scores_gen.go: every row of the game's `score` data
// table, keyed by score id, with the row's scoring type, value and points. The Alliance Duel's
// per-day hero.event.info.get reply lists today's scoring ids (`score`); this table turns an id into
// "what action scores" (e.g. type 51 value 7 = one minute of construction speed-up), which is how
// the real client gates its duel prompts (AllianceCompeteDataManager.lua:678-701). The same file
// holds the two other tables the duel features read from rows: every `lw_hero` row (quality,
// max_rank, max_honorLevel) and every `officer_recruit` row (type, time_type, wish_list); see
// tables.go. Both are read from the same dump directory as score.json unless -heroes / -recruits
// name other files, so the one-table command line below keeps working. Regenerate after a table
// update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/genduelscores -in <tables>/score.json -version <table version> -out internal/game/duel_scores_gen.go
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
	"path/filepath"
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
	value  string
	points int64
}

func main() {
	in := flag.String("in", "", "path to the score table's JSON dump (score.json)")
	version := flag.String("version", "", "table version the dump came from, recorded in the header (e.g. 39432)")
	heroesPath := flag.String("heroes", "", "path to lw_hero.json (default: lw_hero.json next to -in)")
	recruitsPath := flag.String("recruits", "", "path to officer_recruit.json (default: officer_recruit.json next to -in)")
	out := flag.String("out", "duel_scores_gen.go", "output file")
	flag.Parse()
	if *in == "" || *version == "" {
		log.Fatal("genduelscores: -in and -version are required")
	}
	if *heroesPath == "" {
		*heroesPath = filepath.Join(filepath.Dir(*in), "lw_hero.json")
	}
	if *recruitsPath == "" {
		*recruitsPath = filepath.Join(filepath.Dir(*in), "officer_recruit.json")
	}
	rows, err := parseFile(*in, parse)
	if err != nil {
		log.Fatal(err)
	}
	heroes, err := parseFile(*heroesPath, parseHeroes)
	if err != nil {
		log.Fatal(err)
	}
	recruits, err := parseFile(*recruitsPath, parseRecruits)
	if err != nil {
		log.Fatal(err)
	}
	code, err := render(rows, heroes, recruits, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genduelscores: wrote %d score, %d lw_hero and %d officer_recruit rows to %s", len(rows), len(heroes), len(recruits), *out)
}

// parseFile reads path and parses it with fn.
func parseFile[T any](path string, fn func([]byte) ([]T, error)) ([]T, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rows, err := fn(src)
	if err != nil {
		return nil, fmt.Errorf("genduelscores: %s: %w", path, err)
	}
	return rows, nil
}

// knownRows are (id, type, value, points) facts from DUEL.md §2 that the dump must reproduce, so a
// shifted column or a different table can't silently produce a wrong gate.
var knownRows = []row{
	{id: 90402, typ: 82, value: "1", points: 10000}, // complete 1 radar task
	{id: 90201, typ: 51, value: "7", points: 50},    // 1 min construction speed-up
	{id: 90301, typ: 51, value: "6", points: 50},    // 1 min research speed-up
	{id: 90501, typ: 51, value: "4", points: 50},    // 1 min training speed-up
	{id: 90601, typ: 51, value: "9", points: 50},    // 1 min healing speed-up
	{id: 90101, typ: 42, value: "1", points: 1500},  // 1 hero recruitment
}

func parse(src []byte) ([]row, error) {
	t, err := decodeTable(src, "score")
	if err != nil {
		return nil, err
	}
	cols := map[string]int{}
	for _, name := range []string{"type", "value", "points"} {
		c, err := column(t, name)
		if err != nil {
			return nil, err
		}
		cols[name] = c
	}
	var rows []row
	for i, r := range t.Rows {
		if len(r) != 2 {
			return nil, fmt.Errorf("row %d: want [key, values], got %d elements", i, len(r))
		}
		id, ok, err := number(r[0])
		if err != nil || !ok || id <= 0 || id > math.MaxInt32 {
			return nil, fmt.Errorf("row %d: bad key %s", i, r[0])
		}
		var vals []json.RawMessage
		if err := json.Unmarshal(r[1], &vals); err != nil {
			return nil, fmt.Errorf("row %d: %v", i, err)
		}
		typ, _, err := cell(vals, cols["type"])
		if err != nil {
			return nil, fmt.Errorf("row %d type: %v", id, err)
		}
		points, _, err := cell(vals, cols["points"])
		if err != nil {
			return nil, fmt.Errorf("row %d points: %v", id, err)
		}
		value, err := text(vals, cols["value"])
		if err != nil {
			return nil, fmt.Errorf("row %d value: %v", id, err)
		}
		rows = append(rows, row{id: id, typ: typ, value: value, points: points})
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
			return nil, fmt.Errorf("score %d = %+v, want %+v (wrong table or shifted column)", k.id, got, k)
		}
	}
	return rows, nil
}

// decodeTable decodes a JSON dump and checks its table name.
func decodeTable(src []byte, name string) (table, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var t table
	if err := dec.Decode(&t); err != nil {
		return t, err
	}
	if t.Name != name {
		return t, fmt.Errorf("table name %q, want %s", t.Name, name)
	}
	return t, nil
}

// column returns name's 0-based position. A linked (interned) column is refused: its cells are
// indexes into the module's string pool unless the dumper resolved them (linkedColumn).
func column(t table, name string) (int, error) {
	pos, linked, err := columnSpec(t, name)
	if err != nil {
		return 0, err
	}
	if linked {
		return 0, fmt.Errorf("%s is a linked column", name)
	}
	return pos, nil
}

// linkedColumn returns the position of a column that may be linked; the caller must reject a
// numeric cell in it (an unresolved pool index) with resolvedText.
func linkedColumn(t table, name string) (int, error) {
	pos, _, err := columnSpec(t, name)
	return pos, err
}

func columnSpec(t table, name string) (pos int, linked bool, err error) {
	spec, ok := t.Index[name]
	if !ok || len(spec) < 1 {
		return 0, false, fmt.Errorf("no %s column in index", name)
	}
	p, ok, err := number(spec[0])
	if err != nil || !ok || p < 1 {
		return 0, false, fmt.Errorf("%s: bad column position %s", name, spec[0])
	}
	return int(p - 1), len(spec) > 2 && string(spec[2]) == "true", nil
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

func render(rows []row, heroes []heroRow, recruits []recruitRow, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genduelscores from the score, lw_hero and officer_recruit data tables (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	b.WriteString("// duelScoreTable holds every score table row, keyed by score id: typ is the scoring action\n")
	b.WriteString("// (ScoreType, EnumType.lua:15799-15809; 51 = speed-up minutes, 82 = radar task, 42 = hero\n")
	b.WriteString("// recruit, ...), value narrows it (for type 51 the queue: 4 training, 6 research, 7 build, 9 heal)\n")
	fmt.Fprintf(&b, "// and points is the score per unit before tech bonuses. %d rows.\n", len(rows))
	b.WriteString("var duelScoreTable = map[int32]duelScoreRow{\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "\t%d: {typ: %d, value: %s, points: %d},\n", r.id, r.typ, strconv.Quote(strings.TrimSpace(r.value)), r.points)
	}
	b.WriteString("}\n\n")
	renderHeroes(&b, heroes)
	b.WriteString("\n")
	renderRecruits(&b, recruits)
	return format.Source(b.Bytes())
}
