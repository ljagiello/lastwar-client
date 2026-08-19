package main

import "testing"

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
