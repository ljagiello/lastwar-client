package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// heroRow is one lw_hero row as HeroInfo reads it: quality (HeroInfo.lua:132), maxRank
// (tonumber(max_rank) or 0, HeroTemplate.lua:131) and maxHonorLevel (tonumber(max_honorLevel) or 0,
// HeroTemplate.lua:135).
type heroRow struct {
	id, quality, maxRank, maxHonorLevel int64
}

// recruitRow is one officer_recruit row as LotteryInfo:UpdateInfo falls back to it: type and
// time_type when the message has none (LotteryInfo.lua:125-129, 143-147), and the wish list, which
// it always takes from the row (:273-280).
type recruitRow struct {
	id       int64
	typ      int64
	timeType string
	wishList []int64
}

// knownHeroes and knownRecruits are 40413 facts the dumps must reproduce, so a shifted column or a
// different table can't silently produce a wrong gate.
var knownHeroes = []heroRow{
	{id: 10001, quality: 1, maxRank: 26, maxHonorLevel: 600},
	{id: 50006, quality: 5, maxRank: 26, maxHonorLevel: 600},
	{id: 550019, quality: 5, maxRank: 26, maxHonorLevel: 0},
}

var knownRecruits = []recruitRow{
	{id: 221500, typ: 1},
	{id: 221501, typ: 1, wishList: []int64{50006, 50008, 50009, 50016, 50015, 50017, 50019}},
	{id: 301100, typ: 1, timeType: "10"},
	{id: 401100, typ: 9},
}

func parseHeroes(src []byte) ([]heroRow, error) {
	t, err := decodeTable(src, "lw_hero")
	if err != nil {
		return nil, err
	}
	cols, err := columns(t, "quality", "max_rank", "max_honorLevel")
	if err != nil {
		return nil, err
	}
	var rows []heroRow
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, err
		}
		h := heroRow{id: id}
		var ok bool
		if h.quality, ok, err = cell(vals, cols[0]); err != nil || !ok {
			return nil, fmt.Errorf("lw_hero %d: quality missing or not a number", id)
		}
		if h.maxRank, err = textNumber(vals, cols[1]); err != nil {
			return nil, fmt.Errorf("lw_hero %d max_rank: %v", id, err)
		}
		if h.maxHonorLevel, err = textNumber(vals, cols[2]); err != nil {
			return nil, fmt.Errorf("lw_hero %d max_honorLevel: %v", id, err)
		}
		rows = append(rows, h)
	}
	if err := checkKnown(rows, knownHeroes, func(r heroRow) int64 { return r.id }, func(a, b heroRow) bool { return a == b }); err != nil {
		return nil, err
	}
	return rows, nil
}

func parseRecruits(src []byte) ([]recruitRow, error) {
	t, err := decodeTable(src, "officer_recruit")
	if err != nil {
		return nil, err
	}
	cols, err := columns(t, "type", "time_type")
	if err != nil {
		return nil, err
	}
	// wish_list is linked (interned); the dump resolves it to strings.
	wish, err := linkedColumn(t, "wish_list")
	if err != nil {
		return nil, err
	}
	var rows []recruitRow
	for i, r := range t.Rows {
		id, vals, err := rowValues(i, r)
		if err != nil {
			return nil, err
		}
		rr := recruitRow{id: id}
		if rr.typ, err = textNumber(vals, cols[0]); err != nil {
			return nil, fmt.Errorf("officer_recruit %d type: %v", id, err)
		}
		if rr.timeType, err = text(vals, cols[1]); err != nil {
			return nil, fmt.Errorf("officer_recruit %d time_type: %v", id, err)
		}
		list, err := resolvedText(vals, wish)
		if err != nil {
			return nil, fmt.Errorf("officer_recruit %d wish_list: %v", id, err)
		}
		for part := range strings.SplitSeq(list, "|") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil || n <= 0 || n > math.MaxInt32 {
				return nil, fmt.Errorf("officer_recruit %d wish_list: bad hero id %q", id, part)
			}
			rr.wishList = append(rr.wishList, n)
		}
		rows = append(rows, rr)
	}
	if err := checkKnown(rows, knownRecruits, func(r recruitRow) int64 { return r.id }, func(a, b recruitRow) bool {
		return a.id == b.id && a.typ == b.typ && a.timeType == b.timeType && slices.Equal(a.wishList, b.wishList)
	}); err != nil {
		return nil, err
	}
	return rows, nil
}

func columns(t table, names ...string) ([]int, error) {
	out := make([]int, 0, len(names))
	for _, name := range names {
		c, err := column(t, name)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// rowValues splits one [key, [values]] row; the key must be a positive Int.
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

// textNumber reads a number the Lua tonumber()s, given as a number or a decimal string; empty or
// null is 0 (`tonumber(x) or 0`).
func textNumber(vals []json.RawMessage, col int) (int64, error) {
	s, err := text(vals, col)
	if err != nil || strings.TrimSpace(s) == "" {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// resolvedText reads a linked string cell: a JSON string or null. A number there is an unresolved
// string-pool index.
func resolvedText(vals []json.RawMessage, col int) (string, error) {
	if col >= len(vals) {
		return "", nil
	}
	if b := bytes.TrimSpace(vals[col]); len(b) > 0 && b[0] != '"' && string(b) != "null" {
		return "", fmt.Errorf("unresolved linked cell %s", b)
	}
	return text(vals, col)
}

// checkKnown sorts rows by id, refuses a repeated id and checks every known row.
func checkKnown[T any](rows []T, known []T, id func(T) int64, equal func(a, b T) bool) error {
	if len(rows) == 0 {
		return errors.New("no rows")
	}
	slices.SortFunc(rows, func(a, b T) int { return cmp.Compare(id(a), id(b)) })
	byID := map[int64]T{}
	for _, r := range rows {
		if _, dup := byID[id(r)]; dup {
			return fmt.Errorf("row %d repeats", id(r))
		}
		byID[id(r)] = r
	}
	for _, k := range known {
		if got, ok := byID[id(k)]; !ok || !equal(got, k) {
			return fmt.Errorf("row %d = %+v, want %+v (wrong table or shifted column)", id(k), got, k)
		}
	}
	return nil
}

func renderHeroes(b *bytes.Buffer, rows []heroRow) {
	b.WriteString("// duelHeroTable holds every lw_hero row, keyed by hero id, with the fields HeroInfo takes from\n")
	b.WriteString("// the row (HeroInfo.lua:125-145; HeroTemplate.lua:131, 135): quality, maxRank (max_rank) and\n")
	fmt.Fprintf(b, "// maxHonorLevel (max_honorLevel, 0 when empty). %d rows.\n", len(rows))
	b.WriteString("var duelHeroTable = map[int32]duelHeroRow{\n")
	for _, r := range rows {
		fmt.Fprintf(b, "\t%d: {quality: %d, maxRank: %d, maxHonorLevel: %d},\n", r.id, r.quality, r.maxRank, r.maxHonorLevel)
	}
	b.WriteString("}\n")
}

func renderRecruits(b *bytes.Buffer, rows []recruitRow) {
	b.WriteString("// duelRecruitTable holds every officer_recruit row, keyed by pool id, with the fields\n")
	b.WriteString("// LotteryInfo:UpdateInfo takes from the row: typ and timeType when the message has none, and the\n")
	fmt.Fprintf(b, "// wish list (LotteryInfo.lua:125-147, 273-280). %d rows.\n", len(rows))
	b.WriteString("var duelRecruitTable = map[string]duelRecruitRow{\n")
	for _, r := range rows {
		fmt.Fprintf(b, "\t%q: {typ: %d", strconv.FormatInt(r.id, 10), r.typ)
		if r.timeType != "" {
			fmt.Fprintf(b, ", timeType: %q", r.timeType)
		}
		if len(r.wishList) > 0 {
			ids := make([]string, len(r.wishList))
			for i, h := range r.wishList {
				ids[i] = strconv.FormatInt(h, 10)
			}
			fmt.Fprintf(b, ", wishList: []int32{%s}", strings.Join(ids, ", "))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")
}
