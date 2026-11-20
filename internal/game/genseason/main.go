// Command genseason writes internal/game/season_tables_gen.go: the season table facts the season
// features gate on, which the client reads from its bundled tables and never from the wire.
//
//   - lw_season: per season config id (init playerServerSeasonInfo.seasonConfigId) the season
//     number, the map type and type1 (SeasonMapType, SeasonInfoTemplate.lua:228-259), the
//     achievements v2 group (SeasonUtil.lua:1166-1174), the trends group and the golden tree row.
//   - season_achievements: id, group, flag and not_show (SeasonRewardDataManager.lua:49-93).
//   - lw_season_military_level: the rank-up thresholds per group (SeasonMilitaryManager.lua:80-110,
//     216-239, 316-341), and table activity's para_2 of each SeasonMilitary (389) row, the group
//     the activity reads (:220).
//   - lw_season_trends: season_group, unlock_time and para2 (the target) per trend
//     (LWSeasonTrendsData.lua:21-38; LWSeasonTrendsItem.lua:229-230).
//   - season_blood_night_plan: plan id -> flag (BloodyNightDataManager.lua:96-111;
//     BloodyNightData.lua:187-195).
//   - season_golden_tree: id -> cycle (SeasonGoldTreeManager.lua:24-33).
//
// Regenerate after a table update with:
//
//	go generate ./internal/game   (with LASTWAR_TABLES and LASTWAR_TABLE_VERSION set)
//
// or directly:
//
//	go run ./internal/game/genseason -season <tables>/lw_season.json -achievements <tables>/season_achievements.json -military <tables>/lw_season_military_level.json -trends <tables>/lw_season_trends.json -activity <tables>/activity.json -bloodplan <tables>/season_blood_night_plan.json -goldtree <tables>/season_golden_tree.json -version <table version> -out internal/game/season_tables_gen.go
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

// sheet is a decoded table: its rows by key, with column lookups by name.
type sheet struct {
	name string
	cols map[string]int
	keys []int64
	vals map[int64][]json.RawMessage
}

type configRow struct{ id, season, typ, type1, achievements, trends, goldenTree int64 }

type achievementRow struct{ id, group, flag, notShow int64 }

type militaryRow struct {
	group, level, unlockScore, condType, condPara, standard, needRank int64
}

type trendRow struct{ id, group, unlockTime, target int64 }

type pair struct{ id, val int64 }

type cycleRow struct {
	id    int64
	cycle string
}

type tables struct {
	configs      []configRow
	achievements []achievementRow
	military     []militaryRow
	militaryActs []pair // activity id -> para_2
	trends       []trendRow
	bloodPlans   []pair // plan id -> flag
	goldenTrees  []cycleRow
}

func main() {
	paths := map[string]*string{
		"season":       flag.String("season", "", "path to lw_season.json"),
		"achievements": flag.String("achievements", "", "path to season_achievements.json"),
		"military":     flag.String("military", "", "path to lw_season_military_level.json"),
		"trends":       flag.String("trends", "", "path to lw_season_trends.json"),
		"activity":     flag.String("activity", "", "path to activity.json"),
		"bloodplan":    flag.String("bloodplan", "", "path to season_blood_night_plan.json"),
		"goldtree":     flag.String("goldtree", "", "path to season_golden_tree.json"),
	}
	version := flag.String("version", "", "table version the dumps came from, recorded in the header (e.g. 40413)")
	out := flag.String("out", "season_tables_gen.go", "output file")
	flag.Parse()
	src := map[string][]byte{}
	for name, p := range paths {
		if *p == "" {
			log.Fatalf("genseason: -%s is required", name)
		}
		b, err := os.ReadFile(*p)
		if err != nil {
			log.Fatal(err)
		}
		src[name] = b
	}
	if *version == "" {
		log.Fatal("genseason: -version is required")
	}
	t, err := parseAll(src)
	if err != nil {
		log.Fatalf("genseason: %v", err)
	}
	code, err := render(t, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genseason: wrote %d season configs, %d achievements, %d military levels, %d trends to %s",
		len(t.configs), len(t.achievements), len(t.military), len(t.trends), *out)
}

// Known rows of tables 40413 the dumps must reproduce, so a shifted column or another table can't
// silently regate the season features.
var (
	knownConfigs = []configRow{
		{id: 3, season: 1, typ: 1, trends: 10},                                         // Desert: achievements v1
		{id: 1075, season: 6, typ: 6, type1: 1, achievements: 8000, trends: 60},        // S6 NineNationRainforest
		{id: 1477, season: 7, typ: 4, type1: 1, achievements: 9000, trends: 70},        // S7 MummyV2
		{id: 85, season: 4, typ: 5, achievements: 5000, trends: 40, goldenTree: 10001}, // S4 Darkness
	}
	knownAchievements = []achievementRow{{id: 7003, group: 7000, flag: 1, notShow: 1}, {id: 9001, group: 9000}}
	knownMilitary     = []militaryRow{
		{group: 60, level: 2, unlockScore: 12000},
		{group: 70, level: 15, unlockScore: 704420, standard: 1},
		{group: 70, level: 16, needRank: 1},
	}
	knownMilitaryActs = []pair{{1200101, 60}, {1200224, 70}}
	knownTrends       = []trendRow{{id: 1, group: 10, unlockTime: 1, target: 5}, {id: 623, group: 70, unlockTime: 43, target: 100000}}
	knownBloodPlans   = []pair{{1001, 4}}
	knownGoldenTrees  = []cycleRow{{10001, "2|6"}}
)

// actTypeSeasonMilitary is EnumActivity.SeasonMilitary (EnumType.lua:6295-6525).
const actTypeSeasonMilitary = 389

func parseAll(src map[string][]byte) (tables, error) {
	var t tables
	sheets := map[string]*sheet{}
	for flagName, table := range map[string]string{
		"season": "lw_season", "achievements": "season_achievements", "military": "lw_season_military_level",
		"trends": "lw_season_trends", "activity": "activity", "bloodplan": "season_blood_night_plan",
		"goldtree": "season_golden_tree",
	} {
		s, err := decode(src[flagName], table)
		if err != nil {
			return t, fmt.Errorf("%s: %v", table, err)
		}
		sheets[flagName] = s
	}
	var err error
	if t.configs, err = parseConfigs(sheets["season"]); err != nil {
		return t, err
	}
	if t.achievements, err = parseAchievements(sheets["achievements"]); err != nil {
		return t, err
	}
	if t.military, err = parseMilitary(sheets["military"]); err != nil {
		return t, err
	}
	if t.militaryActs, err = parseMilitaryActs(sheets["activity"]); err != nil {
		return t, err
	}
	if t.trends, err = parseTrends(sheets["trends"]); err != nil {
		return t, err
	}
	if t.bloodPlans, err = parsePairs(sheets["bloodplan"], "flag", knownBloodPlans); err != nil {
		return t, err
	}
	if t.goldenTrees, err = parseGoldenTrees(sheets["goldtree"]); err != nil {
		return t, err
	}
	return t, nil
}

func parseConfigs(s *sheet) ([]configRow, error) {
	var out []configRow
	err := s.each(func(id int64, c cells) error {
		r := configRow{id: id}
		for col, dst := range map[string]*int64{
			"season": &r.season, "type": &r.typ, "type1": &r.type1, "season_achievements": &r.achievements,
			"trends": &r.trends, "season_golden_tree": &r.goldenTree,
		} {
			v, err := c.num(col)
			if err != nil {
				return err
			}
			*dst = v
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, knownConfigs, func(r configRow) int64 { return r.id })
}

func parseAchievements(s *sheet) ([]achievementRow, error) {
	var out []achievementRow
	err := s.each(func(id int64, c cells) error {
		r := achievementRow{id: id}
		for col, dst := range map[string]*int64{"group": &r.group, "flag": &r.flag, "not_show": &r.notShow} {
			v, err := c.num(col)
			if err != nil {
				return err
			}
			*dst = v
		}
		if r.group <= 0 {
			return fmt.Errorf("row %d has no group", id)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, knownAchievements, func(r achievementRow) int64 { return r.id })
}

func parseMilitary(s *sheet) ([]militaryRow, error) {
	var out []militaryRow
	err := s.each(func(id int64, c cells) error {
		var r militaryRow
		for col, dst := range map[string]*int64{
			"group": &r.group, "level": &r.level, "unlock_score": &r.unlockScore,
			"unlock_condition_type": &r.condType, "unlock_condition_para": &r.condPara,
			"standard_level": &r.standard, "need_rank": &r.needRank,
		} {
			v, err := c.num(col)
			if err != nil {
				return err
			}
			*dst = v
		}
		if r.group <= 0 || r.level <= 0 {
			return fmt.Errorf("row %d: group %d level %d", id, r.group, r.level)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b militaryRow) int {
		return cmp.Or(cmp.Compare(a.group, b.group), cmp.Compare(a.level, b.level))
	})
	for i := 1; i < len(out); i++ {
		if out[i].group == out[i-1].group && out[i].level == out[i-1].level {
			return nil, fmt.Errorf("%s: group %d has level %d twice", s.name, out[i].group, out[i].level)
		}
	}
	for _, k := range knownMilitary {
		i := slices.IndexFunc(out, func(r militaryRow) bool { return r.group == k.group && r.level == k.level })
		if i < 0 || out[i] != k {
			return nil, fmt.Errorf("%s: group %d level %d is not %+v (wrong table or shifted column)", s.name, k.group, k.level, k)
		}
	}
	return out, nil
}

// parseMilitaryActs reads para_2 of every SeasonMilitary row of table activity.
func parseMilitaryActs(s *sheet) ([]pair, error) {
	var out []pair
	err := s.each(func(id int64, c cells) error {
		typ, err := c.num("type")
		if err != nil || typ != actTypeSeasonMilitary {
			return err
		}
		group, err := c.num("para_2")
		if err != nil {
			return err
		}
		if group <= 0 {
			return fmt.Errorf("SeasonMilitary row %d has no para_2", id)
		}
		out = append(out, pair{id, group})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, knownMilitaryActs, func(p pair) int64 { return p.id })
}

func parseTrends(s *sheet) ([]trendRow, error) {
	var out []trendRow
	err := s.each(func(id int64, c cells) error {
		r := trendRow{id: id}
		for col, dst := range map[string]*int64{"season_group": &r.group, "unlock_time": &r.unlockTime, "para2": &r.target} {
			v, err := c.num(col)
			if err != nil {
				return err
			}
			*dst = v
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, knownTrends, func(r trendRow) int64 { return r.id })
}

func parsePairs(s *sheet, col string, known []pair) ([]pair, error) {
	var out []pair
	err := s.each(func(id int64, c cells) error {
		v, err := c.num(col)
		if err != nil {
			return err
		}
		out = append(out, pair{id, v})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, known, func(p pair) int64 { return p.id })
}

func parseGoldenTrees(s *sheet) ([]cycleRow, error) {
	var out []cycleRow
	err := s.each(func(id int64, c cells) error {
		v, err := c.str("cycle")
		if err != nil {
			return err
		}
		out = append(out, cycleRow{id, strings.TrimSpace(v)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, checkKnown(s.name, out, knownGoldenTrees, func(r cycleRow) int64 { return r.id })
}

// checkKnown fails unless every known row is in rows, equal.
func checkKnown[T comparable](name string, rows, known []T, id func(T) int64) error {
	if len(rows) == 0 {
		return fmt.Errorf("%s: no rows", name)
	}
	for _, k := range known {
		i := slices.IndexFunc(rows, func(r T) bool { return id(r) == id(k) })
		if i < 0 || rows[i] != k {
			var got any = "missing"
			if i >= 0 {
				got = rows[i]
			}
			return fmt.Errorf("%s: row %d = %+v, want %+v (wrong table or shifted column)", name, id(k), got, k)
		}
	}
	return nil
}

func decode(src []byte, name string) (*sheet, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var t table
	if err := dec.Decode(&t); err != nil {
		return nil, err
	}
	if t.Name != name {
		return nil, fmt.Errorf("table name %q, want %s", t.Name, name)
	}
	s := &sheet{name: name, cols: map[string]int{}, vals: map[int64][]json.RawMessage{}}
	for col, spec := range t.Index {
		if len(spec) < 1 {
			return nil, fmt.Errorf("column %s has no position", col)
		}
		pos, ok, err := number(spec[0])
		if err != nil || !ok || pos < 1 {
			return nil, fmt.Errorf("column %s: bad position %s", col, spec[0])
		}
		s.cols[col] = int(pos - 1)
	}
	idCol, hasID := s.cols["id"]
	for i, r := range t.Rows {
		if len(r) != 2 {
			return nil, fmt.Errorf("row %d: want [key, values], got %d elements", i, len(r))
		}
		key, ok, err := number(r[0])
		if err != nil || !ok || key <= 0 || key > math.MaxInt32 {
			return nil, fmt.Errorf("row %d: bad key %s", i, r[0])
		}
		var vals []json.RawMessage
		if err := json.Unmarshal(r[1], &vals); err != nil {
			return nil, fmt.Errorf("row %d: %v", key, err)
		}
		if hasID {
			id, ok, err := cell(vals, idCol)
			if err != nil || !ok || id != key {
				return nil, fmt.Errorf("row %d: id cell is not the key", key)
			}
		}
		if _, dup := s.vals[key]; dup {
			return nil, fmt.Errorf("row %d twice", key)
		}
		s.keys = append(s.keys, key)
		s.vals[key] = vals
	}
	slices.Sort(s.keys)
	if len(s.keys) == 0 {
		return nil, errors.New("no rows")
	}
	return s, nil
}

// cells is one row with its sheet, for lookups by column name.
type cells struct {
	s    *sheet
	key  int64
	vals []json.RawMessage
}

// each calls fn for every row in key order.
func (s *sheet) each(fn func(id int64, c cells) error) error {
	for _, k := range s.keys {
		if err := fn(k, cells{s, k, s.vals[k]}); err != nil {
			return fmt.Errorf("%s: %v", s.name, err)
		}
	}
	return nil
}

// num reads a whole-number cell (a JSON number or a decimal string; null or "" is 0), the
// client's checknumber/toInt of it.
func (c cells) num(col string) (int64, error) {
	pos, ok := c.s.cols[col]
	if !ok {
		return 0, fmt.Errorf("no %s column", col)
	}
	v, _, err := cell(c.vals, pos)
	if err != nil {
		return 0, fmt.Errorf("row %d %s: %v", c.key, col, err)
	}
	return v, nil
}

// str reads a cell as a string (numbers in decimal, null as "").
func (c cells) str(col string) (string, error) {
	pos, ok := c.s.cols[col]
	if !ok {
		return "", fmt.Errorf("no %s column", col)
	}
	v, err := text(c.vals, pos)
	if err != nil {
		return "", fmt.Errorf("row %d %s: %v", c.key, col, err)
	}
	return v, nil
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
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, err
	}
	if n > math.MaxInt32 || n < math.MinInt32 {
		return 0, false, fmt.Errorf("%d does not fit an Int", n)
	}
	return n, true, nil
}

func render(t tables, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genseason from the season tables (table version %s); DO NOT EDIT.\n\n", version)
	b.WriteString("package game\n\n")

	b.WriteString(`// seasonConfigRow is one row of table lw_season, the season config init
// playerServerSeasonInfo.seasonConfigId names (SeasonInfoTemplate.lua:18, 69-72): season is the
// season number, typ and type1 the SeasonMapType and its subdivision (SeasonInfoTemplate.lua:
// 228-259; SeasonUtil.lua:379-384), achievements the achievements v2 group (season_achievements,
// SeasonUtil.lua:1166-1174), trends the lw_season_trends season_group and goldenTree the
// season_golden_tree row (SeasonGoldTreeTemplateManager.lua:23-30). An empty cell is 0.
type seasonConfigRow struct {
	season, typ, type1, achievements, trends, goldenTree int32
}

// seasonAchievementRow is one row of table season_achievements: group is the lw_season
// season_achievements value it belongs to, flag 0 personal, 1 alliance, 2 camp
// (SeasonAchivementFlag, EnumType.lua:15413-15417) and notShow 1 for rows the v2 panel hides
// (LWSeasonPersonalRewardNew.lua:126-136).
type seasonAchievementRow struct {
	id, group, flag, notShow int32
}

// seasonMilitaryLevelRow is one row of table lw_season_military_level: the score that unlocks the
// level by a manual rank-up, its extra condition (type 0 = none), standardLevel 1 on the highest
// manual level and needRank 1 on the levels only the rank grants (SeasonMilitaryManager.lua:80-110,
// 316-341).
type seasonMilitaryLevelRow struct {
	level, unlockScore, conditionType, conditionPara, standardLevel, needRank int32
}

// seasonTrendRow is one row of table lw_season_trends: its season_group, unlock_time (the season
// day it opens, LWSeasonTrendsData.lua:24-25) and para2, the count the claim needs
// (LWSeasonTrendsItem.lua:229-230).
type seasonTrendRow struct {
	group, unlockTime, target int32
}

`)
	fmt.Fprintf(&b, "// seasonConfigTable holds all %d rows of table lw_season, keyed by id.\n", len(t.configs))
	b.WriteString("var seasonConfigTable = map[int32]seasonConfigRow{\n")
	for _, r := range t.configs {
		fmt.Fprintf(&b, "\t%d: {", r.id)
		writeFields(&b, []string{"season", "typ", "type1", "achievements", "trends", "goldenTree"},
			[]int64{r.season, r.typ, r.type1, r.achievements, r.trends, r.goldenTree})
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// seasonAchievementTable holds all %d rows of table season_achievements, by id.\n", len(t.achievements))
	b.WriteString("var seasonAchievementTable = []seasonAchievementRow{\n")
	for _, r := range t.achievements {
		b.WriteString("\t{")
		writeFields(&b, []string{"id", "group", "flag", "notShow"}, []int64{r.id, r.group, r.flag, r.notShow})
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// seasonMilitaryLevels holds all %d rows of table lw_season_military_level by group, in level order.\n", len(t.military))
	b.WriteString("var seasonMilitaryLevels = map[int32][]seasonMilitaryLevelRow{\n")
	group := int64(-1)
	for _, r := range t.military {
		if r.group != group {
			if group >= 0 {
				b.WriteString("\t},\n")
			}
			group = r.group
			fmt.Fprintf(&b, "\t%d: {\n", group)
		}
		b.WriteString("\t\t{")
		writeFields(&b, []string{"level", "unlockScore", "conditionType", "conditionPara", "standardLevel", "needRank"},
			[]int64{r.level, r.unlockScore, r.condType, r.condPara, r.standard, r.needRank})
		b.WriteString("},\n")
	}
	b.WriteString("\t},\n}\n\n")

	b.WriteString("// seasonMilitaryActivityGroups is table activity's para_2 of every SeasonMilitary (389) row: the\n")
	b.WriteString("// lw_season_military_level group the activity reads (SeasonMilitaryManager.lua:216-239).\n")
	b.WriteString("var seasonMilitaryActivityGroups = map[int32]int32{\n")
	for _, p := range t.militaryActs {
		fmt.Fprintf(&b, "\t%d: %d,\n", p.id, p.val)
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// seasonTrendTable holds all %d rows of table lw_season_trends, keyed by trend id.\n", len(t.trends))
	b.WriteString("var seasonTrendTable = map[int32]seasonTrendRow{\n")
	for _, r := range t.trends {
		fmt.Fprintf(&b, "\t%d: {", r.id)
		writeFields(&b, []string{"group", "unlockTime", "target"}, []int64{r.group, r.unlockTime, r.target})
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// seasonBloodNightPlans holds all %d rows of table season_blood_night_plan as plan id -> flag\n", len(t.bloodPlans))
	b.WriteString("// (the bloodNightPlanInfo.groupId the plan belongs to, BloodyNightDataManager.lua:96-111).\n")
	b.WriteString("var seasonBloodNightPlans = map[int32]int32{\n")
	for _, p := range t.bloodPlans {
		fmt.Fprintf(&b, "\t%d: %d,\n", p.id, p.val)
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// seasonGoldenTreeCycles holds all %d rows of table season_golden_tree as id -> cycle\n", len(t.goldenTrees))
	b.WriteString("// (\"start|end\" weekday indexes, SeasonGoldTreeManager.lua:24-33).\n")
	b.WriteString("var seasonGoldenTreeCycles = map[int32]string{\n")
	for _, r := range t.goldenTrees {
		fmt.Fprintf(&b, "\t%d: %s,\n", r.id, strconv.Quote(r.cycle))
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

// writeFields writes the non-zero fields as "name: value" pairs.
func writeFields(b *bytes.Buffer, names []string, vals []int64) {
	first := true
	for i, v := range vals {
		if v == 0 {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		fmt.Fprintf(b, "%s: %d", names[i], v)
	}
}
