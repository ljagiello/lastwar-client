package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

// ItemInfo:UpdateInfo sets an item's id from itemId, then goodsId, and its count from count, then
// num, then itemLeftCnt, so the last present field wins (ItemInfo.lua:43-67). These pin that
// order at the three opt-in claims that read init items through duelItemFields.

func TestLettersReadItemsAsItemInfo(t *testing.T) {
	in := evInit(30)
	a := sfs.NewSFSArray()
	for _, e := range [][]any{
		{"itemId", "670007", "num", 1, "itemLeftCnt", 0},      // itemLeftCnt wins: none left
		{"itemId", "670008", "num", 0, "itemLeftCnt", 1},      // itemLeftCnt wins: one left
		{"itemId", "670009", "goodsId", "999999", "count", 1}, // goodsId wins: not a letter
		{"itemId", "999998", "goodsId", "670010", "count", 1}, // goodsId wins: a letter
		{"itemId", "670002"}, // no count: never in the bag
	} {
		a.AddSFSObject(obj(append([]any{"uuid", "u-" + e[1].(string), "otherPara", `{"state":0}`}, e...)...))
	}
	in.Raw.PutSFSArray("items", a)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runLetters(conn, in); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, r := range fake.requests() {
		sent = append(sent, r.Params.GetString("itemId"))
	}
	if !slices.Equal(sent, []string{"670008", "670010"}) {
		t.Errorf("claimed %v, want [670008 670010]", sent)
	}
}

func TestCitySoldiersSparkItemReadAsItemInfo(t *testing.T) {
	for _, c := range []struct {
		name string
		item []any
		want bool
	}{
		{"itemLeftCnt after num, one left", []any{"itemId", 666701, "num", 0, "itemLeftCnt", 1}, true},
		{"itemLeftCnt after num, none left", []any{"itemId", 666701, "num", 1, "itemLeftCnt", 0}, false},
		{"goodsId names another item", []any{"itemId", 666701, "goodsId", 999999, "count", 1}, false},
		{"goodsId names the upgrade item", []any{"itemId", 999999, "goodsId", 666701, "count", 1}, true},
	} {
		in := evInit(30)
		in.Raw.PutInt("sparkUpgrade", 1) // level 1 upgrades with item 666701
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj(append([]any{"uuid", "u-1"}, c.item...)...))
		in.Raw.PutSFSArray("items", a)
		if _, _, got := citySoldiersSparkUpgradable(in); got != c.want {
			t.Errorf("%s: upgradable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSurvivorItemCountReadsGoodsIDAsItemInfo(t *testing.T) {
	for _, c := range []struct {
		name string
		item []any
		want int64
	}{
		{"goodsId names another item", []any{"itemId", "993113", "goodsId", "1", "count", 5}, 0},
		{"goodsId names the score item", []any{"itemId", "1", "goodsId", "993113", "count", 5}, 5},
	} {
		in := evInit(30)
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj(append([]any{"uuid", "u-1"}, c.item...)...))
		in.Raw.PutSFSArray("items", a)
		if got := survivorItemCount(in, "993113"); got != c.want {
			t.Errorf("%s: survivorItemCount = %d, want %d", c.name, got, c.want)
		}
	}
}
