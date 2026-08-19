package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestBuildingNameOf(t *testing.T) {
	if got := BuildingNameOf(BuildingFarmland); got != "Farmland" {
		t.Errorf("BuildingNameOf(BuildingFarmland) = %q, want %q", got, "Farmland")
	}
	if got := BuildingNameOf(999999999); got != "(unknown type 999999999)" {
		t.Errorf("BuildingNameOf(unknown) = %q, want the unknown-type fallback", got)
	}
}

func TestCollectCmdFor(t *testing.T) {
	if cmd, ok := collectCmdFor(BuildingFarmland); !ok || cmd != "building.production.collect" {
		t.Errorf("collectCmdFor(BuildingFarmland) = (%q, %v), want (\"building.production.collect\", true)", cmd, ok)
	}
	if _, ok := collectCmdFor(999999999); ok {
		t.Errorf("collectCmdFor(unknown type) = ok, want !ok")
	}
}

func TestParseInitBuildings(t *testing.T) {
	t.Run("well-formed entries are kept", func(t *testing.T) {
		b1 := NewSFSObject()
		b1.PutLong("uuid", 111)
		b1.PutInt("bId", BuildingFarmland)
		b2 := NewSFSObject()
		b2.PutLong("uuid", 222)
		b2.PutInt("bId", BuildingIronMine)
		arr := NewSFSArray()
		arr.AddSFSObject(b1)
		arr.AddSFSObject(b2)
		params := NewSFSObject()
		params.PutSFSArray("building_new", arr)

		got := ParseInitBuildings(params)
		if len(got) != 2 {
			t.Fatalf("got %d buildings, want 2", len(got))
		}
		if got[0].Uuid() != 111 || got[1].Uuid() != 222 {
			t.Errorf("unexpected uuids: %d, %d", got[0].Uuid(), got[1].Uuid())
		}
	})
	t.Run("entry missing uuid is skipped, not included as a zero-value target", func(t *testing.T) {
		bad := NewSFSObject()
		bad.PutInt("bId", BuildingFarmland) // no uuid field
		good := NewSFSObject()
		good.PutLong("uuid", 333)
		good.PutInt("bId", BuildingIronMine)
		arr := NewSFSArray()
		arr.AddSFSObject(bad)
		arr.AddSFSObject(good)
		params := NewSFSObject()
		params.PutSFSArray("building_new", arr)

		got := ParseInitBuildings(params)
		if len(got) != 1 {
			t.Fatalf("got %d buildings, want 1 (the malformed entry should be skipped)", len(got))
		}
		if got[0].Uuid() != 333 {
			t.Errorf("got uuid %d, want 333", got[0].Uuid())
		}
	})
	t.Run("field missing entirely", func(t *testing.T) {
		params := NewSFSObject()
		if got := ParseInitBuildings(params); len(got) != 0 {
			t.Errorf("got %d buildings, want 0", len(got))
		}
	})
}

// TestParseInitBuildingsCapsRawItemsExaminedNotJustValidOutput is this round's regression test for
// ParseInitBuildings' raw-item-scan cap (buildings.go's maxRawBuildingItemsPerPush), the buildings.go
// analogue of visitors.go's round-26 ParseInitVisitors fix and its own regression test
// (TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput, visitors_orchestration_test.go) --
// same technique: count actual logged warnings, not len(out), since len(out) stays 0 either way once
// every entry is malformed.
//
// Before this round's fix, ParseInitBuildings had NO cap of any kind on the building_new scan --
// neither an output-count bound nor a raw-item-scan bound (buildings.go was, per this round's audit,
// worse off than visitors.go's own pre-round-26 state, which at least had an output cap). So a
// server-supplied building_new array padded with malformed entries (missing the required "uuid"
// field) would be scanned in full, with requirePresentField logging a Warn for every single one --
// unbounded by anything except sfsobject.go's much larger maxDecodedNodes=300,000 decode budget.
// ParseInitBuildings feeds the PRIMARY init-push path (login.go's waitForInitPush, called from
// Login() on every login), so this was reachable on every single login, not just FetchBuildings'
// fallback path.
//
// Mutation check: reverting ParseInitBuildings' `for i, item := range arr.items { if i >=
// maxRawBuildingItemsPerPush { break }; ... }` back to a plain `for _, item := range arr.items {
// ... }` (no cap at all) makes this test fail with a logged-warning count of wantMalformed instead of
// maxRawBuildingItemsPerPush.
func TestParseInitBuildingsCapsRawItemsExaminedNotJustValidOutput(t *testing.T) {
	const wantMalformed = maxRawBuildingItemsPerPush + 500 // far more malformed entries than the cap

	arr := NewSFSArray()
	for i := 0; i < wantMalformed; i++ {
		bad := NewSFSObject()
		bad.PutInt("bId", BuildingFarmland) // deliberately no "uuid" field
		arr.AddSFSObject(bad)
	}
	params := NewSFSObject()
	params.PutSFSArray("building_new", arr)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := ParseInitBuildings(params)
	slog.SetDefault(orig)

	if len(got) != 0 {
		t.Fatalf("ParseInitBuildings parsed %d buildings, want 0 (every entry in this test is malformed -- missing uuid)", len(got))
	}

	gotWarnings := strings.Count(buf.String(), "skipping building_new entry with no uuid field")
	if gotWarnings != maxRawBuildingItemsPerPush {
		t.Errorf("ParseInitBuildings logged %d \"missing uuid\" warnings, want exactly %d (the cap on RAW items examined, not just valid ones appended) -- input had %d malformed entries; the loop must stop scanning after the first %d regardless of how many turned out valid", gotWarnings, maxRawBuildingItemsPerPush, wantMalformed, maxRawBuildingItemsPerPush)
	}

	if logged := buf.String(); !strings.Contains(logged, "building_new longer than raw-item scan cap; truncating") {
		t.Errorf("expected a truncation warning log from ParseInitBuildings, got:\n%s", logged)
	}
}

// TestFetchBuildingsPushAddBuildingCapsRawItemsExamined covers the "push.add.building" case's own
// inline loop in FetchBuildings (buildings.go) -- one of the two sibling loops that share
// ParseInitBuildings' maxRawBuildingItemsPerPush cap (see that constant's doc comment above
// ParseInitBuildings in buildings.go). Before this round's fix, this loop -- like ParseInitBuildings
// and the "push.init.build" case's defaultBuilds loop -- had no cap of any kind. Uses the same
// newPipeGameConnPair/SendExtension fake-server pattern as buildings_orchestration_test.go's other
// FetchBuildings tests (conn_wait_test.go), and the same "count logged warnings, not len(out)"
// technique as the ParseInitBuildings test above, since every entry here is deliberately malformed.
func TestFetchBuildingsPushAddBuildingCapsRawItemsExamined(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const wantMalformed = maxRawBuildingItemsPerPush + 500 // far more malformed entries than the cap

	done := make(chan struct{})
	go func() {
		defer close(done)
		params := NewSFSObject()
		arr := NewSFSArray()
		for i := 0; i < wantMalformed; i++ {
			bad := NewSFSObject()
			bad.PutInt("bId", BuildingFarmland) // deliberately no "uuid" field
			arr.AddSFSObject(bad)
		}
		params.PutSFSArray("buildings", arr)
		_ = server.SendExtension("push.add.building", params)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	buildings, _, err := FetchBuildings(client, 150*time.Millisecond)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending push.add.building")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a plain timeout after partial data is not itself an error)", err)
	}
	if len(buildings) != 0 {
		t.Fatalf("got %d buildings, want 0 (every entry in this test is malformed -- missing uuid)", len(buildings))
	}

	gotWarnings := strings.Count(buf.String(), "skipping push.add.building entry with no uuid field")
	if gotWarnings != maxRawBuildingItemsPerPush {
		t.Errorf("push.add.building's inline loop logged %d \"missing uuid\" warnings, want exactly %d (the cap on RAW items examined) -- input had %d malformed entries; the loop must stop scanning after the first %d regardless of how many turned out valid", gotWarnings, maxRawBuildingItemsPerPush, wantMalformed, maxRawBuildingItemsPerPush)
	}
}

// TestFetchBuildingsPushInitBuildCapsRawItemsExamined covers the "push.init.build" case's own
// inline loop in FetchBuildings (buildings.go) -- the THIRD of the three sibling loops that share
// maxRawBuildingItemsPerPush (see that constant's doc comment above ParseInitBuildings in
// buildings.go). Round 27 added the cap to all three loops (ParseInitBuildings' building_new scan,
// this "push.init.build"/defaultBuilds scan, and "push.add.building"/buildings scan) but only wrote
// regression tests for the first and third -- see
// TestParseInitBuildingsCapsRawItemsExaminedNotJustValidOutput and
// TestFetchBuildingsPushAddBuildingCapsRawItemsExamined above, whose technique (and fake-server
// pattern, buildings_orchestration_test.go/conn_wait_test.go's newPipeGameConnPair/SendExtension)
// this test mirrors exactly, just for the middle loop.
//
// Each defaultBuilds entry here is a wrapper carrying a buildInfo object with no "uuid" field (see
// buildings.go's push.init.build case: it reads `wrapper.Get("buildInfo")` first, then checks
// requireFieldType on THAT nested object, not the wrapper itself), so every entry is malformed and
// len(buildings) stays 0 throughout the scan either way -- counting the actually-logged "missing
// uuid" warnings, not len(buildings), is what makes this test capable of catching an
// unbounded-scan regression at all.
//
// No "init" push is sent in this test, so gotAuthoritativeInit never becomes true and
// push.init.build's own deadline-shrink (gated on it) never fires -- FetchBuildings simply waits
// out the short fixed timeout passed below, same as TestFetchBuildingsPushAddBuildingCapsRawItemsExamined
// does for its own sibling loop.
//
// Mutation check: reverting the "push.init.build" case's `for i, item := range arr.items { if i >=
// maxRawBuildingItemsPerPush { break }; ... }` in buildings.go back to a plain `for _, item := range
// arr.items { ... }` (no cap at all) makes this test fail with a logged-warning count of
// wantMalformed instead of maxRawBuildingItemsPerPush.
func TestFetchBuildingsPushInitBuildCapsRawItemsExamined(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const wantMalformed = maxRawBuildingItemsPerPush + 500 // far more malformed entries than the cap

	done := make(chan struct{})
	go func() {
		defer close(done)
		params := NewSFSObject()
		arr := NewSFSArray()
		for i := 0; i < wantMalformed; i++ {
			bad := NewSFSObject()
			bad.PutInt("bId", BuildingFarmland) // deliberately no "uuid" field
			wrapper := NewSFSObject()
			wrapper.PutSFSObject("buildInfo", bad)
			arr.AddSFSObject(wrapper)
		}
		params.PutSFSArray("defaultBuilds", arr)
		_ = server.SendExtension("push.init.build", params)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	buildings, _, err := FetchBuildings(client, 150*time.Millisecond)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending push.init.build")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a plain timeout after partial data is not itself an error)", err)
	}
	if len(buildings) != 0 {
		t.Fatalf("got %d buildings, want 0 (every entry in this test is malformed -- missing uuid)", len(buildings))
	}

	gotWarnings := strings.Count(buf.String(), "skipping push.init.build entry with no uuid field")
	if gotWarnings != maxRawBuildingItemsPerPush {
		t.Errorf("push.init.build's inline loop logged %d \"missing uuid\" warnings, want exactly %d (the cap on RAW items examined) -- input had %d malformed entries; the loop must stop scanning after the first %d regardless of how many turned out valid", gotWarnings, maxRawBuildingItemsPerPush, wantMalformed, maxRawBuildingItemsPerPush)
	}
	if !strings.Contains(buf.String(), "push.init.build defaultBuilds longer than raw-item scan cap; truncating") {
		t.Errorf("expected a truncation warning from push.init.build's inline loop, got:\n%s", buf.String())
	}
}

// TestParseInitBuildingsWrongTypedUUIDIsRejected is the round-28 regression test for
// requireFieldType (buildings.go): before this round's fix, requirePresentField only checked that
// a field was present and non-nil, never that its concrete decoded SFS type actually matched what
// Building.Uuid() (GetLong) accepts. A present-but-wrong-typed uuid (e.g. the server sending it as
// a string instead of a Long) silently passed that presence-only guard and then coerced to
// int64(0) via GetLong's own zero-value fallback -- indistinguishable from a genuine uuid=0 and
// exactly as dangerous: dedupeBuildings (login.go, the PRIMARY init-push path) and FetchBuildings'
// seenBuildingUUIDs would treat it as a duplicate of any other zero/wrong-typed uuid, silently
// dropping one of two otherwise-distinct real buildings.
//
// The input here has a wrong-typed uuid entry (a string) and a separate, genuinely-well-typed
// uuid=0 entry -- proving these two are no longer conflated: exactly one building must come back
// (the genuine uuid=0 one), and a "wrong-typed uuid" warning, not a "missing uuid" one, must be
// logged for the string entry.
//
// Mutation check: reverting ParseInitBuildings' `requireFieldType(bi, "uuid", "building_new",
// sfsFieldKindLong)` back to `requirePresentField(bi, "uuid", "building_new")` makes this test fail
// with 2 buildings instead of 1 (both uuid=0, indistinguishable), and no "wrong-typed" warning
// logged.
func TestParseInitBuildingsWrongTypedUUIDIsRejected(t *testing.T) {
	wrongTyped := NewSFSObject()
	wrongTyped.PutUtfString("uuid", "not-a-long") // wrong SFS type: a uuid must be a Long
	wrongTyped.PutInt("bId", BuildingFarmland)

	genuineZero := NewSFSObject()
	genuineZero.PutLong("uuid", 0) // a real, well-typed uuid that happens to be zero
	genuineZero.PutInt("bId", BuildingIronMine)

	arr := NewSFSArray()
	arr.AddSFSObject(wrongTyped)
	arr.AddSFSObject(genuineZero)
	params := NewSFSObject()
	params.PutSFSArray("building_new", arr)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := ParseInitBuildings(params)
	slog.SetDefault(orig)

	if len(got) != 1 {
		t.Fatalf("ParseInitBuildings parsed %d buildings, want 1 (only the genuine, well-typed uuid=0 entry -- the string-typed one must be rejected, not silently coerced to uuid=0 too)", len(got))
	}
	if got[0].Uuid() != 0 || got[0].BId() != BuildingIronMine {
		t.Errorf("got building uuid=%d bId=%d, want the genuine uuid=0 bId=%d entry", got[0].Uuid(), got[0].BId(), BuildingIronMine)
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping building_new entry with wrong-typed uuid field") {
		t.Errorf("expected a wrong-typed-uuid warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping building_new entry with no uuid field") {
		t.Errorf("wrong-typed uuid must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

// TestParseInitBuildingsWrongTypedBIdIsRejected is the round-29 regression test for the bId guard
// added to ParseInitBuildings' population loop (buildings.go), mirroring
// TestParseInitBuildingsWrongTypedUUIDIsRejected above exactly, just for bId instead of uuid. Before
// this round's fix, a present-but-wrong-typed bId (e.g. sent as a string) silently coerced to bId=0
// via BId()'s GetInt zero-value fallback -- not a correctness bug (collectCmdFor(0) never matches
// any known building type, so the building was already excluded from CollectAll's collect loop
// either way -- see this codebase's own fail-safe framing), but with zero diagnostic signal that the
// entry was actually malformed rather than a genuine, if useless, bId=0. This guard is purely
// consistency/diagnosability, matching the sibling uuid guard immediately above it in buildings.go.
func TestParseInitBuildingsWrongTypedBIdIsRejected(t *testing.T) {
	wrongTyped := NewSFSObject()
	wrongTyped.PutLong("uuid", 111)
	wrongTyped.PutUtfString("bId", "not-an-int") // wrong SFS type: bId must be an Int

	genuineZero := NewSFSObject()
	genuineZero.PutLong("uuid", 222)
	genuineZero.PutInt("bId", 0) // a real, well-typed bId that happens to be zero (an unknown type)

	arr := NewSFSArray()
	arr.AddSFSObject(wrongTyped)
	arr.AddSFSObject(genuineZero)
	params := NewSFSObject()
	params.PutSFSArray("building_new", arr)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := ParseInitBuildings(params)
	slog.SetDefault(orig)

	if len(got) != 1 {
		t.Fatalf("ParseInitBuildings parsed %d buildings, want 1 (only the genuine, well-typed bId=0 entry -- the string-typed one must be rejected, not silently coerced to bId=0 too)", len(got))
	}
	if got[0].Uuid() != 222 || got[0].BId() != 0 {
		t.Errorf("got building uuid=%d bId=%d, want the genuine uuid=222 bId=0 entry", got[0].Uuid(), got[0].BId())
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping building_new entry with wrong-typed bId field") {
		t.Errorf("expected a wrong-typed-bId warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping building_new entry with no bId field") {
		t.Errorf("wrong-typed bId must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

// TestParseInitBuildingsRawItemCapBoundary is the round-28 boundary-condition regression test for
// maxRawBuildingItemsPerPush: every prior raw-item-scan-cap test for this constant (see
// TestParseInitBuildingsCapsRawItemsExaminedNotJustValidOutput above) overshoots the cap by a wide
// margin (+500), so none would catch a production ">"-to-">=" regression in the truncation-warning
// condition (`len(arr.items) > maxRawBuildingItemsPerPush`) -- an off-by-one there would either warn
// one item too early, or worse, silently drop the last legitimate entry of an exactly-at-cap push
// without ever logging why. This test drives both sides of that boundary directly with well-formed
// entries, so len(got) itself -- not just a logged-warning count -- proves whether the cap fired at
// the right point.
func TestParseInitBuildingsRawItemCapBoundary(t *testing.T) {
	buildParams := func(n int) *SFSObject {
		arr := NewSFSArray()
		for i := 0; i < n; i++ {
			arr.AddSFSObject(newTestBuildingSFS(int64(i), BuildingFarmland, 1))
		}
		params := NewSFSObject()
		params.PutSFSArray("building_new", arr)
		return params
	}

	t.Run("exactly cap items: all parsed, no truncation warning", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := ParseInitBuildings(buildParams(maxRawBuildingItemsPerPush))
		slog.SetDefault(orig)

		if len(got) != maxRawBuildingItemsPerPush {
			t.Fatalf("got %d buildings, want exactly %d (the cap, all well-formed)", len(got), maxRawBuildingItemsPerPush)
		}
		if strings.Contains(buf.String(), "longer than raw-item scan cap") {
			t.Errorf("unexpected truncation warning at exactly-cap boundary:\n%s", buf.String())
		}
	})

	t.Run("cap+1 items: truncation warning fires, only cap parsed", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := ParseInitBuildings(buildParams(maxRawBuildingItemsPerPush + 1))
		slog.SetDefault(orig)

		if len(got) != maxRawBuildingItemsPerPush {
			t.Fatalf("got %d buildings, want exactly %d (cap+1 input must still truncate to the cap)", len(got), maxRawBuildingItemsPerPush)
		}
		if !strings.Contains(buf.String(), "building_new longer than raw-item scan cap; truncating") {
			t.Errorf("expected a truncation warning at cap+1, got:\n%s", buf.String())
		}
	})
}

// TestFetchBuildingsPushAddBuildingWrongTypedUUIDIsRejected is the round-30 regression test for
// requireFieldType's uuid guard as used by FetchBuildings' "push.add.building" inline loop
// (buildings.go) -- the second of the three sibling loops sharing the identical uuid+bId guard
// pair (ParseInitBuildings' building_new loop, this push.add.building loop, and the
// push.init.build/defaultBuilds loop). Before this round, a wrong-typed-uuid/bId regression test
// existed for only the first of the three (ParseInitBuildings, via
// TestParseInitBuildingsWrongTypedUUIDIsRejected above); this closes that gap for one of the other
// two, mirroring that test's technique exactly -- a wrong-typed (string) uuid entry alongside a
// genuinely well-typed uuid=0 entry, proving the two are not conflated (a wrong-typed uuid must not
// silently coerce to uuid=0 via GetLong and collide with a genuine uuid=0 building) -- but drives it
// through a live push.add.building push via FetchBuildings, the same newPipeGameConnPair/
// SendExtension fake-server pattern TestFetchBuildingsPushAddBuildingCapsRawItemsExamined above uses
// for this same loop.
func TestFetchBuildingsPushAddBuildingWrongTypedUUIDIsRejected(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		wrongTyped := NewSFSObject()
		wrongTyped.PutUtfString("uuid", "not-a-long") // wrong SFS type: a uuid must be a Long
		wrongTyped.PutInt("bId", BuildingFarmland)

		genuineZero := NewSFSObject()
		genuineZero.PutLong("uuid", 0) // a real, well-typed uuid that happens to be zero
		genuineZero.PutInt("bId", BuildingIronMine)

		arr := NewSFSArray()
		arr.AddSFSObject(wrongTyped)
		arr.AddSFSObject(genuineZero)
		params := NewSFSObject()
		params.PutSFSArray("buildings", arr)
		_ = server.SendExtension("push.add.building", params)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	buildings, _, err := FetchBuildings(client, 150*time.Millisecond)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending push.add.building")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil", err)
	}
	if len(buildings) != 1 {
		t.Fatalf("got %d buildings, want 1 (only the genuine, well-typed uuid=0 entry -- the string-typed one must be rejected, not silently coerced to uuid=0 too)", len(buildings))
	}
	if buildings[0].Uuid() != 0 || buildings[0].BId() != BuildingIronMine {
		t.Errorf("got building uuid=%d bId=%d, want the genuine uuid=0 bId=%d entry", buildings[0].Uuid(), buildings[0].BId(), BuildingIronMine)
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping push.add.building entry with wrong-typed uuid field") {
		t.Errorf("expected a wrong-typed-uuid warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping push.add.building entry with no uuid field") {
		t.Errorf("wrong-typed uuid must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

// TestFetchBuildingsPushAddBuildingWrongTypedBIdIsRejected is TestFetchBuildingsPushAddBuildingWrongTypedUUIDIsRejected's
// sibling for the bId half of the identical guard pair, mirroring
// TestParseInitBuildingsWrongTypedBIdIsRejected's technique the same way its uuid counterpart above
// mirrors TestParseInitBuildingsWrongTypedUUIDIsRejected's.
func TestFetchBuildingsPushAddBuildingWrongTypedBIdIsRejected(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		wrongTyped := NewSFSObject()
		wrongTyped.PutLong("uuid", 111)
		wrongTyped.PutUtfString("bId", "not-an-int") // wrong SFS type: bId must be an Int

		genuineZero := NewSFSObject()
		genuineZero.PutLong("uuid", 222)
		genuineZero.PutInt("bId", 0) // a real, well-typed bId that happens to be zero (an unknown type)

		arr := NewSFSArray()
		arr.AddSFSObject(wrongTyped)
		arr.AddSFSObject(genuineZero)
		params := NewSFSObject()
		params.PutSFSArray("buildings", arr)
		_ = server.SendExtension("push.add.building", params)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	buildings, _, err := FetchBuildings(client, 150*time.Millisecond)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending push.add.building")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil", err)
	}
	if len(buildings) != 1 {
		t.Fatalf("got %d buildings, want 1 (only the genuine, well-typed bId=0 entry -- the string-typed one must be rejected, not silently coerced to bId=0 too)", len(buildings))
	}
	if buildings[0].Uuid() != 222 || buildings[0].BId() != 0 {
		t.Errorf("got building uuid=%d bId=%d, want the genuine uuid=222 bId=0 entry", buildings[0].Uuid(), buildings[0].BId())
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping push.add.building entry with wrong-typed bId field") {
		t.Errorf("expected a wrong-typed-bId warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping push.add.building entry with no bId field") {
		t.Errorf("wrong-typed bId must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

func TestParseInitVisitors(t *testing.T) {
	t.Run("well-formed entry kept, missing-uid entry skipped", func(t *testing.T) {
		bad := NewSFSObject()
		bad.PutInt("eventId", 2005) // no uid field
		good := NewSFSObject()
		good.PutLong("uid", 444)
		good.PutInt("eventId", 2001)
		list := NewSFSArray()
		list.AddSFSObject(bad)
		list.AddSFSObject(good)
		visitor := NewSFSObject()
		visitor.PutSFSArray("list", list)
		params := NewSFSObject()
		params.PutSFSObject("visitor", visitor)

		got := ParseInitVisitors(params)
		if len(got) != 1 {
			t.Fatalf("got %d visitors, want 1", len(got))
		}
		if got[0].Uid() != 444 {
			t.Errorf("got uid %d, want 444", got[0].Uid())
		}
	})
}

// TestSfsFieldKindAccepts directly, exhaustively table-tests sfsFieldKindAccepts (buildings.go) --
// the type-check function backing every requireFieldType call site across this codebase
// (buildings.go/alliance.go/visitors.go/mail.go) -- against its full documented contract, rather
// than only ever being exercised incidentally through requireFieldType at 2 of its 4 accepted
// concrete Go types.
//
// Round-29 gap this closes: every existing wrong-typed-field regression test in this codebase
// (TestParseInitBuildingsWrongTypedUUIDIsRejected, TestFindRecommendedTechWrongTypedScienceIdIsRejected,
// TestParseInitVisitorsWrongTypedUIDIsRejected, and mail_orchestration_test.go's two sibling tests,
// which this file doesn't own) only ever drives a STRING value against an int64/int32-expecting
// field, or vice versa -- never int16 or byte, even though sfsFieldKindAccepts' own switch
// (buildings.go) explicitly lists "int64, int32, int16, byte" as accepted for BOTH
// sfsFieldKindLong and sfsFieldKindInt. A regression narrowing that switch's case list (e.g.
// dropping int16/byte, or a typo'd case) would compile cleanly and pass every one of those
// call-site tests untouched, since none of them ever construct an int16 or byte value -- this test
// is the only thing that would catch it.
//
// sfsFieldKindLong and sfsFieldKindInt are documented (buildings.go's sfsFieldKind doc comments) to
// accept the IDENTICAL four-type set, mirroring GetLong/GetInt's own identical accepted sets -- so
// both are driven through the same four-type table below, plus sfsFieldKindString's own single
// accepted type (string), and one genuinely-wrong-typed value per kind (bool and float64, neither
// of which any accessor's own type switch -- sfsobject.go -- ever accepts) proving the false path
// is real, not just an assumption from the absence of a true case.
func TestSfsFieldKindAccepts(t *testing.T) {
	// wrongTyped is shared across all three kinds below: bool and float64 are not in ANY
	// accessor's accepted set (sfsobject.go's GetLong/GetInt/GetString), so both must be rejected
	// by every kind, not just the one under test.
	wrongTyped := []interface{}{true, float64(3.14)}

	t.Run("sfsFieldKindLong", func(t *testing.T) {
		for _, v := range []interface{}{int64(1), int32(1), int16(1), byte(1)} {
			if !sfsFieldKindAccepts(sfsFieldKindLong, v) {
				t.Errorf("sfsFieldKindAccepts(sfsFieldKindLong, %#v) = false, want true (%T is in GetLong's accepted set)", v, v)
			}
		}
		for _, v := range wrongTyped {
			if sfsFieldKindAccepts(sfsFieldKindLong, v) {
				t.Errorf("sfsFieldKindAccepts(sfsFieldKindLong, %#v) = true, want false (%T is not in GetLong's accepted set)", v, v)
			}
		}
	})

	t.Run("sfsFieldKindInt", func(t *testing.T) {
		for _, v := range []interface{}{int64(1), int32(1), int16(1), byte(1)} {
			if !sfsFieldKindAccepts(sfsFieldKindInt, v) {
				t.Errorf("sfsFieldKindAccepts(sfsFieldKindInt, %#v) = false, want true (%T is in GetInt's accepted set)", v, v)
			}
		}
		for _, v := range wrongTyped {
			if sfsFieldKindAccepts(sfsFieldKindInt, v) {
				t.Errorf("sfsFieldKindAccepts(sfsFieldKindInt, %#v) = true, want false (%T is not in GetInt's accepted set)", v, v)
			}
		}
	})

	t.Run("sfsFieldKindString", func(t *testing.T) {
		if !sfsFieldKindAccepts(sfsFieldKindString, "hello") {
			t.Error("sfsFieldKindAccepts(sfsFieldKindString, \"hello\") = false, want true (string is GetString's sole accepted type)")
		}
		// int64 stands in for the numeric kinds' own accepted types here, proving
		// sfsFieldKindString doesn't accidentally accept what sfsFieldKindLong/sfsFieldKindInt do.
		for _, v := range append([]interface{}{int64(1)}, wrongTyped...) {
			if sfsFieldKindAccepts(sfsFieldKindString, v) {
				t.Errorf("sfsFieldKindAccepts(sfsFieldKindString, %#v) = true, want false (%T is not in GetString's accepted set)", v, v)
			}
		}
	})
}
