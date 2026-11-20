package game

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestGeneratedTablesMatchTheDump reruns every generator of this package, exactly as its
// //go:generate line runs it, against the table dump in LASTWAR_TABLES (and the decompiled Lua in
// LASTWAR_LUA for the generators that read it), and fails when a committed *_gen.go differs from
// what the dump gives. Each table update (39879 -> 40413 added 79 activity rows, 160 building rows,
// 62 dig maps, 33 radar events, ...) otherwise strands the features on the old rows silently
// (cov-tables/F14). Skipped unless LASTWAR_TABLES is set, so plain `go test` needs no dump.
//
// The version a directive takes from LASTWAR_TABLE_VERSION (LASTWAR_LUA_VERSION) is the one stamped
// in the committed file's header, so only content drifts fail; a stamp older than
// LASTWAR_TABLE_VERSION is logged. A directive that needs a variable that is unset is skipped with a
// log line.
func TestGeneratedTablesMatchTheDump(t *testing.T) {
	if os.Getenv("LASTWAR_TABLES") == "" {
		t.Skip("LASTWAR_TABLES is not set (the directory of table JSON dumps, e.g. tables40413_json)")
	}
	directives, err := goGenerateDirectives(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(directives) == 0 {
		t.Fatal("no //go:generate directives found")
	}
	tmp := t.TempDir()
	for _, d := range directives {
		t.Run(d.out, func(t *testing.T) {
			committed, err := os.ReadFile(d.out)
			if err != nil {
				t.Fatalf("%s (from %s): %v", d.out, d.file, err)
			}
			args, missing := d.expand(committed, filepath.Join(tmp, d.out))
			if len(missing) > 0 {
				t.Skipf("%s needs %s, which is unset", d.out, strings.Join(missing, ", "))
			}
			if want := os.Getenv("LASTWAR_TABLE_VERSION"); want != "" {
				if got := headerVersion(committed, tableVersionRE); got != "" && got != want {
					t.Logf("%s is stamped table version %s, LASTWAR_TABLE_VERSION is %s", d.out, got, want)
				}
			}
			cmd := exec.CommandContext(t.Context(), "go", args...)
			cmd.Dir = "."
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			fresh, err := os.ReadFile(filepath.Join(tmp, d.out))
			if err != nil {
				t.Fatal(err)
			}
			if diff := lineDiff(committed, fresh, 8); diff != "" {
				t.Errorf("%s differs from what %s generates from the dump; rerun go generate (with the same version stamp) and update the features that copy its rows:\n%s",
					d.out, d.gen, diff)
			}
		})
	}
}

// genDirective is one `//go:generate go run ./<gen> ...` line.
type genDirective struct {
	file string   // the .go file holding the directive
	gen  string   // ./genactivities etc.
	args []string // the arguments after `go`, unexpanded
	out  string   // the -out file, relative to the package
}

var envRefRE = regexp.MustCompile(`\$\{([A-Z_]+)\}`)

// tableVersionRE and appVersionRE find the stamps generators write into their header comments
// ("(table version 40413)", "table version 40413.", "(app 1.0.370)").
var (
	tableVersionRE = regexp.MustCompile(`table version ([0-9]+)`)
	appVersionRE   = regexp.MustCompile(`\(app ([0-9][0-9.]*)\)`)
)

// goGenerateDirectives reads the //go:generate lines of the non-test .go files in dir.
func goGenerateDirectives(dir string) ([]genDirective, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []genDirective
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(src))
		sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "//go:generate ")
			if !ok {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[0] != "go" || fields[1] != "run" {
				return nil, fmt.Errorf("%s: unexpected directive %q", f, line)
			}
			d := genDirective{file: filepath.Base(f), gen: fields[2], args: fields[1:]}
			for i, a := range d.args {
				if a == "-out" && i+1 < len(d.args) {
					d.out = d.args[i+1]
				} else if v, ok := strings.CutPrefix(a, "-out="); ok {
					d.out = v
				}
			}
			if d.out == "" || strings.Contains(d.out, "$") || filepath.Base(d.out) != d.out {
				return nil, fmt.Errorf("%s: directive %q has no plain -out file", f, line)
			}
			out = append(out, d)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b genDirective) int { return strings.Compare(a.out, b.out) })
	return out, nil
}

// expand returns the directive's arguments with ${VAR}s filled in and -out pointed at outPath. The
// version variables take the committed file's stamps when it has them; missing lists the variables
// that are needed but unset.
func (d genDirective) expand(committed []byte, outPath string) (args, missing []string) {
	vars := map[string]string{}
	lookup := func(name string) string {
		if v, ok := vars[name]; ok {
			return v
		}
		v := os.Getenv(name)
		switch name {
		case "LASTWAR_TABLE_VERSION":
			if s := headerVersion(committed, tableVersionRE); s != "" {
				v = s
			}
		case "LASTWAR_LUA_VERSION":
			if s := headerVersion(committed, appVersionRE); s != "" {
				v = s
			}
		}
		if v == "" && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
		vars[name] = v
		return v
	}
	for i := 0; i < len(d.args); i++ {
		a := d.args[i]
		switch {
		case a == "-out" && i+1 < len(d.args):
			args = append(args, a, outPath)
			i++
			continue
		case strings.HasPrefix(a, "-out="):
			args = append(args, "-out="+outPath)
			continue
		}
		args = append(args, envRefRE.ReplaceAllStringFunc(a, func(m string) string {
			return lookup(envRefRE.FindStringSubmatch(m)[1])
		}))
	}
	return args, missing
}

// headerVersion returns re's first match in a generated file's header comment (the lines before
// the package clause, where every generator here states its versions), "" when none.
func headerVersion(src []byte, re *regexp.Regexp) string {
	head := src
	if i := bytes.Index(src, []byte("\npackage ")); i >= 0 {
		head = src[:i]
	}
	if m := re.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

// lineDiff summarizes how b differs from a, line by line: a count and the first max differing
// lines of each side. "" when they are equal.
func lineDiff(a, b []byte, max int) string {
	if bytes.Equal(a, b) {
		return ""
	}
	la, lb := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	inA, inB := map[string]int{}, map[string]int{}
	for _, l := range la {
		inA[l]++
	}
	for _, l := range lb {
		inB[l]++
	}
	var gone, added []string
	for _, l := range la {
		if inB[l] > 0 {
			inB[l]--
			continue
		}
		gone = append(gone, l)
	}
	for _, l := range lb {
		if inA[l] > 0 {
			inA[l]--
			continue
		}
		added = append(added, l)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d committed lines not generated, %d generated lines not committed (%d vs %d lines)\n", len(gone), len(added), len(la), len(lb))
	for i, l := range gone {
		if i == max {
			fmt.Fprintf(&sb, "  ... %d more\n", len(gone)-max)
			break
		}
		fmt.Fprintf(&sb, "  - %s\n", l)
	}
	for i, l := range added {
		if i == max {
			fmt.Fprintf(&sb, "  ... %d more\n", len(added)-max)
			break
		}
		fmt.Fprintf(&sb, "  + %s\n", l)
	}
	return sb.String()
}

// TestGoGenerateDirectivesAndExpand checks the drift test's plumbing without a dump: every
// generator's directive is found, and expanding one keeps the committed stamps and redirects -out.
func TestGoGenerateDirectivesAndExpand(t *testing.T) {
	ds, err := goGenerateDirectives(".")
	if err != nil {
		t.Fatal(err)
	}
	byOut := map[string]genDirective{}
	for _, d := range ds {
		byOut[d.out] = d
	}
	for _, out := range []string{"activity_tables_gen.go", "alliance_science_gen.go", "dig_tables_gen.go", "duel_scores_gen.go",
		"mail_groups_gen.go", "production_gen.go", "radar_tables_gen.go", "season_tables_gen.go", "visitor_types_gen.go"} {
		if _, ok := byOut[out]; !ok {
			t.Errorf("no //go:generate directive writes %s (found %v)", out, ds)
		}
	}
	t.Setenv("LASTWAR_TABLES", "/dump")
	t.Setenv("LASTWAR_TABLE_VERSION", "99999")
	t.Setenv("LASTWAR_LUA", "")
	t.Setenv("LASTWAR_LUA_VERSION", "")
	activity := []byte("// Code generated by genactivities from the activity data table (table version 40413); DO NOT EDIT.\n\npackage game\n")
	args, missing := byOut["activity_tables_gen.go"].expand(activity, "/tmp/x.go")
	if want := "run ./genactivities -in /dump/activity.json -version 40413 -out /tmp/x.go"; strings.Join(args, " ") != want || len(missing) != 0 {
		t.Errorf("expand = %q, missing %v; want %q", strings.Join(args, " "), missing, want)
	}
	mail := []byte("// Code generated by genmailgroups from MailEnum.lua (app 1.0.370) and table mail_group (table version 40413); DO NOT EDIT.\n\npackage game\n")
	args, missing = byOut["mail_groups_gen.go"].expand(mail, "/tmp/m.go")
	if !slices.Equal(missing, []string{"LASTWAR_LUA"}) || !slices.Contains(args, "-version=1.0.370") || !slices.Contains(args, "-table-version=40413") {
		t.Errorf("mail groups expand = %q, missing %v; want LASTWAR_LUA missing and the committed stamps", args, missing)
	}
	visitor := []byte("// Code generated by internal/game/gen_visitor_types; DO NOT EDIT.\n// Source: client data table lw_base_visitor_event, 398 rows, table version 39879.\n\npackage game\n\nconst y = \"table version 1\"\n")
	if got := headerVersion(visitor, tableVersionRE); got != "39879" {
		t.Errorf("headerVersion = %q, want 39879", got)
	}
	if lineDiff([]byte("a\nb\n"), []byte("a\nb\n"), 8) != "" {
		t.Error("equal files must not differ")
	}
	if d := lineDiff([]byte("a\nb\nc\n"), []byte("a\nc\nd\n"), 8); !strings.Contains(d, "  - b\n") || !strings.Contains(d, "  + d\n") {
		t.Errorf("lineDiff = %q", d)
	}
}

// TestGeneratedTablesShareTableVersion holds the header stamp of every file a //go:generate line
// writes to tableVersion (cov-tables/F14 part 4). A go generate that skips a generator (the mail
// groups also need LASTWAR_LUA and keep their file without it, so table mail_group's newer types
// and their rewards would be left out) or a version bump that forgets the constant fails here,
// without a dump.
func TestGeneratedTablesShareTableVersion(t *testing.T) {
	ds, err := goGenerateDirectives(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) == 0 {
		t.Fatal("no //go:generate directives found")
	}
	for _, d := range ds {
		src, err := os.ReadFile(d.out)
		if err != nil {
			t.Errorf("%s: %v", d.out, err)
			continue
		}
		if got := headerVersion(src, tableVersionRE); got != tableVersion {
			t.Errorf("%s is stamped table version %q, tableVersion is %q: regenerate every table (see tables.go)", d.out, got, tableVersion)
		}
	}
}
