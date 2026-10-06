// Command gen_visitor_types writes internal/game/visitor_types_gen.go: the eventId -> behaviour
// type map GreetVisitors keys its allowlist on, generated from the client data table
// lw_base_visitor_event (TableName.City_Visitor, Global/EnumType.lua:2751).
//
// The table is not in this repository. It comes from a decompiled client build's
// tables/json/lw_base_visitor_event.json, whose rows are positional: rows[i] is [key, cols], and
// the "index" object gives each named column's 1-based position in cols.
//
//	go run ./internal/game/gen_visitor_types -table <dir>/lw_base_visitor_event.json \
//	    -version 39432 -live 39516 -out internal/game/visitor_types_gen.go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"maps"
	"math"
	"os"
	"slices"
)

func main() {
	table := flag.String("table", "", "path to lw_base_visitor_event.json (required)")
	version := flag.String("version", "", "table version the JSON was decompiled from, e.g. 39432 (required)")
	live := flag.String("live", "", "a later live table version the table is byte-identical in, if checked")
	out := flag.String("out", "", "output file (default stdout)")
	flag.Parse()
	if *table == "" || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*table, *version, *live, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen_visitor_types:", err)
		os.Exit(1)
	}
}

func run(tablePath, version, live, outPath string) error {
	f, err := os.Open(tablePath)
	if err != nil {
		return err
	}
	types, err := parseTable(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := checkKnownPairs(types); err != nil {
		return err
	}
	src, err := render(types, version, live)
	if err != nil {
		return err
	}
	if outPath == "" {
		_, err = os.Stdout.Write(src)
		return err
	}
	return os.WriteFile(outPath, src, 0o644)
}

type tableJSON struct {
	Name  string                       `json:"name"`
	Index map[string][]json.RawMessage `json:"index"`
	Rows  [][]json.RawMessage          `json:"rows"`
}

// parseTable returns id -> type for every row of the lw_base_visitor_event JSON in r.
func parseTable(r io.Reader) (map[int32]int32, error) {
	var t tableJSON
	if err := json.NewDecoder(r).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode table: %w", err)
	}
	if t.Name != "lw_base_visitor_event" {
		return nil, fmt.Errorf("table name %q, want lw_base_visitor_event", t.Name)
	}
	idPos, err := columnPos(t.Index, "id")
	if err != nil {
		return nil, err
	}
	typePos, err := columnPos(t.Index, "type")
	if err != nil {
		return nil, err
	}
	out := make(map[int32]int32, len(t.Rows))
	for i, row := range t.Rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("row %d: %d elements, want [key, cols]", i, len(row))
		}
		key, err := intValue(row[0])
		if err != nil {
			return nil, fmt.Errorf("row %d key: %w", i, err)
		}
		var cols []json.RawMessage
		if err := json.Unmarshal(row[1], &cols); err != nil {
			return nil, fmt.Errorf("row %d (%d) cols: %w", i, key, err)
		}
		if len(cols) < max(idPos, typePos) {
			return nil, fmt.Errorf("row %d (%d): %d columns, want at least %d", i, key, len(cols), max(idPos, typePos))
		}
		id, err := intValue(cols[idPos-1])
		if err != nil {
			return nil, fmt.Errorf("row %d (%d) id: %w", i, key, err)
		}
		if id != key {
			return nil, fmt.Errorf("row %d: key %d but id column %d; the column index is off", i, key, id)
		}
		typ, err := intValue(cols[typePos-1])
		if err != nil {
			return nil, fmt.Errorf("row %d (%d) type: %w", i, key, err)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("row %d: duplicate id %d", i, id)
		}
		out[id] = typ
	}
	if len(out) == 0 {
		return nil, errors.New("table has no rows")
	}
	return out, nil
}

// columnPos returns the 1-based position of column name from the table's index ([pos, type]).
func columnPos(index map[string][]json.RawMessage, name string) (int, error) {
	spec, ok := index[name]
	if !ok || len(spec) == 0 {
		return 0, fmt.Errorf("index has no %q column", name)
	}
	pos, err := intValue(spec[0])
	if err != nil || pos < 1 {
		return 0, fmt.Errorf("index %q position %s is not a positive integer", name, spec[0])
	}
	return int(pos), nil
}

func intValue(raw json.RawMessage) (int32, error) {
	var f *float64
	if err := json.Unmarshal(raw, &f); err != nil || f == nil {
		return 0, fmt.Errorf("%s is not a number", raw)
	}
	if *f != math.Trunc(*f) || *f < math.MinInt32 || *f > math.MaxInt32 {
		return 0, fmt.Errorf("%s is not an int32", raw)
	}
	return int32(*f), nil
}

// knownPairs are eventId -> type pairs confirmed independently of the table: live greets of
// 2001-2006 (type 2), 3115/3116 (3) and 20840-20849 (10), and the two merchants 1001/1002 whose
// para is "item;count;resourceType;price" (VisitorMerchant.lua:33-59). A column shift or a
// different table would break at least one of them.
var knownPairs = func() map[int32]int32 {
	m := map[int32]int32{1001: 1, 1002: 1, 3115: 3, 3116: 3}
	for id := int32(2001); id <= 2006; id++ {
		m[id] = 2
	}
	for id := int32(20840); id <= 20849; id++ {
		m[id] = 10
	}
	return m
}()

func checkKnownPairs(types map[int32]int32) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(knownPairs)) {
		got, ok := types[id]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("eventId %d missing, want type %d", id, knownPairs[id]))
		case got != knownPairs[id]:
			errs = append(errs, fmt.Errorf("eventId %d has type %d, want %d", id, got, knownPairs[id]))
		}
	}
	return errors.Join(errs...)
}

func render(types map[int32]int32, version, live string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by internal/game/gen_visitor_types; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "// Source: client data table lw_base_visitor_event, %d rows, table version %s", len(types), version)
	if live != "" {
		fmt.Fprintf(&b, "\n// (byte-identical in live table version %s)", live)
	}
	b.WriteString(".\n\n")
	b.WriteString("package game\n\n")
	b.WriteString("// visitorEventTypes maps a visitor's eventId (lw_base_visitor_event.id) to the table's type\n")
	b.WriteString("// column: the behaviour type (VisitorType, Global/EnumType.lua:11982) the client picks the\n")
	b.WriteString("// visitor's class by (CityVisitorManager.lua:98-129).\n")
	b.WriteString("var visitorEventTypes = map[int32]int32{\n")
	for _, id := range slices.Sorted(maps.Keys(types)) {
		fmt.Fprintf(&b, "\t%d: %d,\n", id, types[id])
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
