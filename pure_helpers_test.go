package main

import "testing"

func TestCollectibleBuildings(t *testing.T) {
	newBuilding := func(uuid int64, bId int32) Building {
		o := NewSFSObject()
		o.PutLong("uuid", uuid)
		o.PutInt("bId", bId)
		return Building{Raw: o}
	}
	buildings := []Building{
		newBuilding(1, BuildingFarmland),
		newBuilding(2, 99999999), // not a recognized collectible type
		newBuilding(3, BuildingIronMine),
	}
	got := collectibleBuildings(buildings)
	if len(got) != 2 {
		t.Fatalf("got %d collectible buildings, want 2", len(got))
	}
	if got[0].Uuid() != 1 || got[1].Uuid() != 3 {
		t.Errorf("unexpected uuids: %d, %d", got[0].Uuid(), got[1].Uuid())
	}
}

func TestGroupUnclaimedByType(t *testing.T) {
	newMail := func(uid string, mailType int32, rewardStatus int32) Mail {
		o := NewSFSObject()
		o.PutUtfString("uid", uid)
		o.PutInt("type", mailType)
		o.PutInt("rewardStatus", rewardStatus)
		return Mail{Raw: o}
	}
	mail := []Mail{
		newMail("a", 3, 0), // unclaimed, type 3
		newMail("b", 3, 1), // already claimed
		newMail("c", 4, 0), // unclaimed, type 4
		newMail("d", 3, 0), // unclaimed, type 3
	}
	got := groupUnclaimedByType(mail)
	if len(got) != 2 {
		t.Fatalf("got %d distinct types, want 2", len(got))
	}
	if len(got[3]) != 2 || got[3][0] != "a" || got[3][1] != "d" {
		t.Errorf("type 3: got %v, want [a d]", got[3])
	}
	if len(got[4]) != 1 || got[4][0] != "c" {
		t.Errorf("type 4: got %v, want [c]", got[4])
	}
}

func TestFindRecommendedTech(t *testing.T) {
	t.Run("no recommended entry", func(t *testing.T) {
		arr := NewSFSArray()
		o := NewSFSObject()
		o.PutInt("scienceId", 100)
		o.PutInt("state", 0)
		arr.AddSFSObject(o)
		_, found := findRecommendedTech(arr)
		if found {
			t.Fatal("expected found=false when no entry has state=1")
		}
	})
	t.Run("exactly one recommended entry", func(t *testing.T) {
		arr := NewSFSArray()
		o1 := NewSFSObject()
		o1.PutInt("scienceId", 100)
		o1.PutInt("state", 0)
		arr.AddSFSObject(o1)
		o2 := NewSFSObject()
		o2.PutInt("scienceId", 200)
		o2.PutInt("state", 1)
		arr.AddSFSObject(o2)
		id, found := findRecommendedTech(arr)
		if !found || id != 200 {
			t.Fatalf("got (id=%d, found=%v), want (200, true)", id, found)
		}
	})
	t.Run("empty array", func(t *testing.T) {
		arr := NewSFSArray()
		_, found := findRecommendedTech(arr)
		if found {
			t.Fatal("expected found=false for an empty array")
		}
	})
}
