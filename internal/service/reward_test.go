package service

import "testing"

func TestRewardCount(t *testing.T) {
	cases := []struct {
		name                                         string
		configured, freeSlots, unusedValid, lifeLeft int
		want                                         int
	}{
		{"configured limits", 3, 5, 0, 20, 3},
		{"one free slot", 3, 1, 0, 20, 1},
		{"unused fill free slots", 3, 2, 2, 20, 0},
		{"unused exceed free slots", 3, 2, 4, 20, 0},
		{"lifetime left 2", 3, 5, 0, 2, 2},
		{"lifetime exhausted", 3, 5, 0, 0, 0},
		{"configured 0", 0, 5, 0, 20, 0},
		{"negative configured", -1, 5, 0, 20, 0},
		{"negative free", 3, -1, 0, 20, 0},
		{"negative unused", 3, 1, -5, 20, 0},
		{"negative lifetime", 3, 5, 0, -2, 0},
	}
	for _, c := range cases {
		if got := RewardCount(c.configured, c.freeSlots, c.unusedValid, c.lifeLeft); got != c.want {
			t.Errorf("%s: RewardCount(%d,%d,%d,%d) = %d, want %d", c.name,
				c.configured, c.freeSlots, c.unusedValid, c.lifeLeft, got, c.want)
		}
	}
}
