// Command genmailgroups writes internal/game/mail_groups_gen.go from the client's MailEnum.lua: the
// internal group of every mail type (MailTypeToInternalGroup) and the groups the mailbox's Read All
// and Claim All walk (MailShowGroup). The client claims mail one group at a time and sends the
// group id as mail.reward.batch's type (MailDataManager.lua:439-462, 615-632;
// MailRewardBatchMessage.lua:3-17), so Go needs the same map. Regenerate after a Lua update with:
//
//	go generate ./internal/game   (with LASTWAR_LUA set to the decompiled Lua root and LASTWAR_LUA_VERSION to its app version)
//
// or directly:
//
//	go run ./internal/game/genmailgroups -lua=<lua root> -version=<app version> -out internal/game/mail_groups_gen.go
//
// With -lua empty (LASTWAR_LUA unset) it keeps the existing file and exits 0, so a go generate run
// that only refreshes the data tables still works.
package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"go/format"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// enumPath is MailEnum.lua's place under the decompiled Lua root.
const enumPath = "DataCenter/MailData/MailEnum.lua"

type entry struct {
	typ, group      int64
	typName, gnName string
}

type parsed struct {
	entries []entry
	show    []int64
	favor   int64
	hide    int64
}

func main() {
	lua := flag.String("lua", "", "decompiled Lua root (the directory holding DataCenter/); empty keeps the existing output")
	version := flag.String("version", "", "app version the Lua came from, recorded in the header (e.g. 1.0.367)")
	out := flag.String("out", "mail_groups_gen.go", "output file")
	flag.Parse()
	if *lua == "" {
		log.Printf("genmailgroups: LASTWAR_LUA is not set; keeping %s", *out)
		return
	}
	if *version == "" {
		log.Fatal("genmailgroups: -version is required with -lua")
	}
	path := filepath.Join(*lua, enumPath)
	src, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	p, err := parse(src)
	if err != nil {
		log.Fatalf("genmailgroups: %s: %v", path, err)
	}
	code, err := render(p, *version)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, code, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("genmailgroups: wrote %d mail types to %s", len(p.entries), *out)
}

var (
	numberLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*) = (-?[0-9]+),?$`)
	groupRef   = regexp.MustCompile(`^MailInternalGroup\.([A-Za-z_][A-Za-z0-9_]*),?$`)
	mapLine    = regexp.MustCompile(`^\[MailType\.([A-Za-z_][A-Za-z0-9_]*)\] = MailInternalGroup\.([A-Za-z_][A-Za-z0-9_]*),?$`)
)

// known are group ids the client and a live capture pin, so a renumbered or misread enum can't
// silently retarget every claim: the capture's Claim All sent types 3, 4 and 9 for the Alliance,
// Event and Season tabs (mail.go), and MailEnum.lua:162-173 numbers the rest.
var known = map[string]int64{
	"MAIL_IN_hide": 1, "MAIL_IN_report": 2, "MAIL_IN_alliance": 3, "MAIL_IN_activity": 4,
	"MAIL_IN_system": 5, "MAIL_IN_daily": 6, "MAIL_IN_charge": 7, "MAIL_IN_favor": 8, "MAIL_IN_season": 9,
}

// knownTypes are mail type numbers Go relies on elsewhere (mailCantClaimAllTypes, MailEnum.lua:53,
// 75-76).
var knownTypes = map[string]int64{"CANT_CLAIM_ALL": 112, "TranslationRating": 134, "Automatic_TranslationRating": 135}

func parse(src []byte) (parsed, error) {
	var p parsed
	types, err := numbers(src, "MailType")
	if err != nil {
		return p, err
	}
	groups, err := numbers(src, "MailInternalGroup")
	if err != nil {
		return p, err
	}
	delete(groups, "MAIL_IN_MAX") // an alias of the highest id, not a group
	for name, id := range known {
		if groups[name] != id {
			return p, fmt.Errorf("MailInternalGroup.%s is %d, want %d", name, groups[name], id)
		}
	}
	for name, n := range knownTypes {
		if types[name] != n {
			return p, fmt.Errorf("MailType.%s is %d, want %d", name, types[name], n)
		}
	}
	p.favor, p.hide = groups["MAIL_IN_favor"], groups["MAIL_IN_hide"]

	show, err := block(src, "MailShowGroup")
	if err != nil {
		return p, err
	}
	for _, l := range show {
		m := groupRef.FindStringSubmatch(l)
		if m == nil {
			return p, fmt.Errorf("MailShowGroup: unexpected line %q", l)
		}
		g, ok := groups[m[1]]
		if !ok {
			return p, fmt.Errorf("MailShowGroup: unknown group %s", m[1])
		}
		p.show = append(p.show, g)
	}
	if len(p.show) == 0 || slices.Contains(p.show, p.hide) {
		return p, fmt.Errorf("MailShowGroup %v: want a non-empty list without the hide group", p.show)
	}

	lines, err := block(src, "MailTypeToInternalGroup")
	if err != nil {
		return p, err
	}
	seen := map[int64]bool{}
	for _, l := range lines {
		m := mapLine.FindStringSubmatch(l)
		if m == nil {
			return p, fmt.Errorf("MailTypeToInternalGroup: unexpected line %q", l)
		}
		t, ok := types[m[1]]
		if !ok {
			return p, fmt.Errorf("MailTypeToInternalGroup: unknown type %s", m[1])
		}
		g, ok := groups[m[2]]
		if !ok {
			return p, fmt.Errorf("MailTypeToInternalGroup: unknown group %s", m[2])
		}
		if seen[t] {
			return p, fmt.Errorf("MailTypeToInternalGroup: type %s (%d) listed twice", m[1], t)
		}
		seen[t] = true
		p.entries = append(p.entries, entry{typ: t, group: g, typName: m[1], gnName: m[2]})
	}
	if len(p.entries) == 0 {
		return p, fmt.Errorf("MailTypeToInternalGroup is empty")
	}
	slices.SortFunc(p.entries, func(a, b entry) int { return cmp.Compare(a.typ, b.typ) })
	return p, nil
}

// numbers reads a `Name = { KEY = n, ... }` table.
func numbers(src []byte, name string) (map[string]int64, error) {
	lines, err := block(src, name)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, l := range lines {
		m := numberLine.FindStringSubmatch(l)
		if m == nil {
			return nil, fmt.Errorf("%s: unexpected line %q", name, l)
		}
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("%s.%s listed twice", name, m[1])
		}
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %v", name, m[1], err)
		}
		out[m[1]] = n
	}
	return out, nil
}

// block returns the trimmed, non-empty lines between `Name = {` at the start of a line and the
// next line that is just `}`.
func block(src []byte, name string) ([]string, error) {
	all := strings.Split(string(src), "\n")
	start := -1
	for i, l := range all {
		if strings.TrimRight(l, " \r") == name+" = {" {
			if start >= 0 {
				return nil, fmt.Errorf("%s is defined twice", name)
			}
			start = i
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no %s table", name)
	}
	var lines []string
	for _, l := range all[start+1:] {
		l = strings.TrimSpace(l)
		if l == "}" {
			return lines, nil
		}
		if l != "" {
			lines = append(lines, l)
		}
	}
	return nil, fmt.Errorf("%s: no closing brace", name)
}

func render(p parsed, version string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by genmailgroups from MailEnum.lua (app %s); DO NOT EDIT.\n\npackage game\n\n", version)
	fmt.Fprintf(&b, "// mailGroupsVersion is the app version whose Lua mailTypeGroup was generated from.\nconst mailGroupsVersion = %q\n\n", version)
	fmt.Fprintf(&b, "// mailTypeGroup is MailTypeToInternalGroup (MailEnum.lua): the internal group, the mailbox tab, of\n"+
		"// each of %d mail types. A type without an entry is in no group, and the client never lists it.\n"+
		"var mailTypeGroup = map[int64]int64{\n", len(p.entries))
	for _, e := range p.entries {
		fmt.Fprintf(&b, "\t%d: %d, // %s: %s\n", e.typ, e.group, e.typName, e.gnName)
	}
	b.WriteString("}\n\n")
	show := make([]string, len(p.show))
	for i, g := range p.show {
		show[i] = strconv.FormatInt(g, 10)
	}
	fmt.Fprintf(&b, "// mailShowGroups is MailShowGroup: the groups the mailbox's Read All and Claim All walk, in that\n"+
		"// order (UILWMailMainCtrl.lua:172-185). The hide group (%d) is not among them.\n"+
		"var mailShowGroups = []int64{%s}\n\n", p.hide, strings.Join(show, ", "))
	fmt.Fprintf(&b, "// mailFavorGroup is MailInternalGroup.MAIL_IN_favor, the group of saved mail (saveFlag 1).\nconst mailFavorGroup = %d\n", p.favor)
	return format.Source(b.Bytes())
}
