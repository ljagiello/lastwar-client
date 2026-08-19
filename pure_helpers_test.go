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
