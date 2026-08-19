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
