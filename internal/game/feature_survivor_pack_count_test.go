package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestSurvivorItemCountFollowsItemInfo: an init item stack's count is the last of count, num and
// itemLeftCnt the stack carries, as ItemInfo:UpdateInfo applies them in that order
// (ItemInfo.lua:52-66), so a stack with count 5 and itemLeftCnt 0 holds none.
func TestSurvivorItemCountFollowsItemInfo(t *testing.T) {
	for _, c := range []struct {
		name   string
		counts []any
		want   int64
	}{
		{"count only", []any{"count", 5}, 5},
		{"itemLeftCnt after count", []any{"count", 5, "itemLeftCnt", 0}, 0},
		{"num after count", []any{"count", 0, "num", 3}, 3},
		{"itemLeftCnt after num", []any{"num", 3, "itemLeftCnt", 2}, 2},
	} {
		in := evInit(30)
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj(append([]any{"uuid", "u-1", "itemId", "993113"}, c.counts...)...))
		in.Raw.PutSFSArray("items", a)
		if got := survivorItemCount(in, "993113"); got != c.want {
			t.Errorf("%s: survivorItemCount = %d, want %d", c.name, got, c.want)
		}
	}
}
