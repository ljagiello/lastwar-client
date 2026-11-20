package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestLettersWire pins what letters sends, as literals from the 1.0.370 client:
// post.card.receive {itemId: UtfString goods id} (MsgDefines.lua:2265; PostCardReceiveMessage.lua:4-7;
// ThanksLetterItem.lua:246, BirthdaySznLetterItem.lua:280), once for each letter goods id.
//
// The bag holds one stack of each of the 12 goods of type 160 in table goods 40413, each with a
// letter that still shows its reward button. 654125 is the VIP 18 invitation (DataConfig
// vip_letter.k2), which opens its own envelope (UIVip18EnvelopView.lua:168) and is never sent; the
// other 11 are claimed in bag order.
func TestLettersWire(t *testing.T) {
	goodsType160 := []string{"654120", "654124", "654125", "670002", "670003", "670004", "670005", "670006", "670007",
		"670008", "670009", "670010"}
	var items [][3]string
	for _, id := range goodsType160 {
		items = append(items, [3]string{id, "1", `{"state":0}`})
	}
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runLetters(conn, letterInit(items...)); err != nil {
		t.Fatal(err)
	}
	var want []dWire
	for _, id := range []string{"654120", "654124", "670002", "670003", "670004", "670005", "670006", "670007", "670008",
		"670009", "670010"} {
		want = append(want, dWire{"post.card.receive", map[string]dParam{"itemId": dStr(id)}})
	}
	dCheckWire(t, fake, want)
}
