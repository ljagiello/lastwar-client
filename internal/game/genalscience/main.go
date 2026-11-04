// Command genalscience writes internal/game/alliance_science_gen.go: the two alliance-tech table
// facts the donation gate needs (alliance.go). The client reads a tech's max level from table
// lw_alliance_science_tab (AllianceScienceDataManager.lua:39, max_lv) and, when the server's
// needPro is 0 or missing, the donation target from table lw_alliance_science_detail at id
// scienceId + level (AllianceScienceData.lua:144-152, AllianceScienceTemplate.lua:61: maxProNum is
// the row's value column). Regenerate after a table update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/genalscience -tab <tables>/lw_alliance_science_tab.json -detail <tables>/lw_alliance_science_detail.json -version <table version> -out internal/game/alliance_science_gen.go
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
)

// table is the JSON dump's shape: index maps column name -> [position, type, linked?], and each
// row is [key, [values...]] with values in column order.
type table struct {
	Name  string                       `json:"name"`
	Index map[string][]json.RawMessage `json:"index"`
	Rows  [][]json.RawMessage          `json:"rows"`
}

// entry is one generated map entry.
type entry struct{ id, val int64 }

func main() {
	tabPath := flag.String("tab", "", "path to lw_alliance_science_tab.json")
	detailPath := flag.String("detail", "", "path to lw_alliance_science_detail.json")
	version := flag.String("version", "", "table version the dumps came from, recorded in the header (e.g. 39879)")
	out := flag.String("out", "alliance_science_gen.go", "output file")
	flag.Parse()
	if *tabPath == "" || *detailPath == "" || *version == "" {
		log.Fatal("genalscience: -tab, -detail and -version are required")
	}
	tab, err := os.ReadFile(*tabPath)
	if err != nil {
		log.Fatal(err)
	}
	detail, err := os.ReadFile(*detailPath)
	if err != nil {
		log.Fatal(err)
	}
	maxLv, err := parseTab(tab)
	if err != nil {
		log.Fatalf("genalscience: %s: %v", *tabPath, err)
	}
	maxPro, err := parseDetail(detail)
	if err != nil {
		log.Fatalf("genalscience: %s: %v", *detailPath, err)
	}
	code, err := render(maxLv, maxPro, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genalscience: wrote %d max levels and %d targets to %s", len(maxLv), len(maxPro), *out)
}

// knownMaxLevels and knownMaxPro are rows of table 39879 the dumps must reproduce, so a shifted
// column or another table can't silently regate every donation.
var (
	knownMaxLevels = []entry{{10011100, 1}, {10011200, 10}}
	knownMaxPro    = []entry{{10011100, 10000}, {10011101, 80000}, {10011200, 100000}}
)

// parseTab reads lw_alliance_science_tab: id -> max_lv.
func parseTab(src []byte) ([]entry, error) {
	t, cols, err := load(src, "lw_alliance_science_tab", "id", "max_lv")
	if err != nil {
		return nil, err
	}
	var out []entry
	err = eachRow(t, cols["id"], func(id int64, vals []json.RawMessage) error {
		lv, ok, err := cell(vals, cols["max_lv"])
		if err != nil || !ok || lv < 0 || lv > math.MaxInt32 {
			return fmt.Errorf("tech %d: bad max_lv %s", id, raw(vals, cols["max_lv"]))
		}
		out = append(out, entry{id, lv})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, check(out, knownMaxLevels, "max_lv")
}

// parseDetail reads lw_alliance_science_detail: id -> value, checking that every id is
// science_id + science_lv, the key GetAlScienceTemplate(scienceId + curLevel) looks up.
func parseDetail(src []byte) ([]entry, error) {
	t, cols, err := load(src, "lw_alliance_science_detail", "id", "science_id", "science_lv", "value")
	if err != nil {
		return nil, err
	}
	var out []entry
	err = eachRow(t, cols["id"], func(id int64, vals []json.RawMessage) error {
		sid, ok1, err1 := cell(vals, cols["science_id"])
		lv, ok2, err2 := cell(vals, cols["science_lv"])
		if err := errors.Join(err1, err2); err != nil || !ok1 || !ok2 || sid+lv != id {
			return fmt.Errorf("row %d: science_id %s + science_lv %s is not the id (wrong table or shifted column)",
				id, raw(vals, cols["science_id"]), raw(vals, cols["science_lv"]))
		}
		v, ok, err := cell(vals, cols["value"])
		if err != nil || !ok || v < 0 {
			return fmt.Errorf("row %d: bad value %s", id, raw(vals, cols["value"]))
		}
		out = append(out, entry{id, v})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, check(out, knownMaxPro, "value")
}

func load(src []byte, name string, columns ...string) (table, map[string]int, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var t table
	if err := dec.Decode(&t); err != nil {
		return t, nil, err
	}
	if t.Name != name {
		return t, nil, fmt.Errorf("table name %q, want %s", t.Name, name)
	}
	cols := map[string]int{}
	for _, c := range columns {
		pos, err := column(t, c)
		if err != nil {
			return t, nil, err
		}
		cols[c] = pos
	}
	return t, cols, nil
}

// eachRow calls fn for every row with its id, checking the key against the id column and
// rejecting duplicates.
func eachRow(t table, idCol int, fn func(id int64, vals []json.RawMessage) error) error {
	seen := map[int64]bool{}
	for i, r := range t.Rows {
		if len(r) != 2 {
			return fmt.Errorf("row %d: want [key, values], got %d elements", i, len(r))
		}
		id, ok, err := number(r[0])
		if err != nil || !ok || id <= 0 || id > math.MaxInt32 {
			return fmt.Errorf("row %d: bad key %s", i, r[0])
		}
		if seen[id] {
			return fmt.Errorf("row %d: duplicate id %d", i, id)
		}
		seen[id] = true
		var vals []json.RawMessage
		if err := json.Unmarshal(r[1], &vals); err != nil {
			return fmt.Errorf("row %d: %v", i, err)
		}
		if own, ok, err := cell(vals, idCol); err != nil || !ok || own != id {
			return fmt.Errorf("row %d: id column is %d (wrong table or shifted column)", id, own)
		}
		if err := fn(id, vals); err != nil {
			return err
		}
	}
	if len(seen) == 0 {
		return errors.New("no rows")
	}
	return nil
}

func check(got, known []entry, what string) error {
	slices.SortFunc(got, func(a, b entry) int { return cmp.Compare(a.id, b.id) })
	byID := map[int64]int64{}
	for _, e := range got {
		byID[e.id] = e.val
	}
	for _, k := range known {
		if v, ok := byID[k.id]; !ok || v != k.val {
			return fmt.Errorf("row %d %s = %d, want %d (wrong table or shifted column)", k.id, what, v, k.val)
		}
	}
	return nil
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

// number reads a JSON number (or numeric string) as an integer; null is ok=false.
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
		s = x
	default:
		return 0, false, fmt.Errorf("unexpected %T", v)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func render(maxLv, maxPro []entry, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genalscience from the alliance science tables (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")
	b.WriteString("// allianceScienceTableVersion is the table version the maps below were generated from.\n")
	fmt.Fprintf(&b, "const allianceScienceTableVersion = %s\n\n", strconv.Quote(version))
	fmt.Fprintf(&b, "// allianceScienceMaxLevel holds all %d rows of table lw_alliance_science_tab as tech id -> max_lv\n", len(maxLv))
	b.WriteString("// (AllianceScienceDataManager.lua:39).\n")
	b.WriteString("var allianceScienceMaxLevel = map[int32]int32{\n")
	for _, e := range maxLv {
		fmt.Fprintf(&b, "\t%d: %d,\n", e.id, e.val)
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "// allianceScienceMaxPro holds all %d rows of table lw_alliance_science_detail as id\n", len(maxPro))
	b.WriteString("// (science_id + science_lv) -> value, the template maxProNum (AllianceScienceTemplate.lua:61).\n")
	b.WriteString("var allianceScienceMaxPro = map[int32]int64{\n")
	for _, e := range maxPro {
		fmt.Fprintf(&b, "\t%d: %d,\n", e.id, e.val)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
