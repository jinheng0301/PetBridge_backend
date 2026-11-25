package service

import (
	"slices"
	"testing"
)

func TestDecorCatalog_Fixed(t *testing.T) {
	want := []DecorItem{
		{"wall_clock", 10}, {"picture_frame", 40}, {"floor_lamp", 80}, {"string_lights", 140},
		{"potted_tree", 220}, {"wall_map", 320}, {"woven_basket", 440}, {"bookshelf", 580},
		{"mailbox", 740}, {"yarn_ball", 920}, {"record_player", 1120}, {"wind_chime", 1340},
	}
	got := DecorCatalog()
	if !slices.Equal(got, want) {
		t.Fatalf("catalog %+v", got)
	}
	seen := map[string]bool{}
	for i, d := range got {
		if seen[d.ID] || (i > 0 && d.Threshold <= got[i-1].Threshold) {
			t.Fatalf("item %d %+v repeats an id or does not raise the threshold", i, d)
		}
		seen[d.ID] = true
	}
	got[0] = DecorItem{"changed", 1}
	if !slices.Equal(DecorCatalog(), want) {
		t.Fatal("changing the returned slice changed the catalog")
	}
}

func TestNextThreshold(t *testing.T) {
	for total, want := range map[int]int{0: 10, 9: 10, 10: 40, 39: 40, 1339: 1340} {
		if got := NextThreshold(total); got == nil || *got != want {
			t.Errorf("NextThreshold(%d)=%v want %d", total, got, want)
		}
	}
	for _, total := range []int{1340, 9999} {
		if got := NextThreshold(total); got != nil {
			t.Errorf("NextThreshold(%d)=%d want nil", total, *got)
		}
	}
}

func TestItemsReached(t *testing.T) {
	if got := ItemsReached(0); got == nil || len(got) != 0 {
		t.Fatalf("0: %v", got)
	}
	if got := ItemsReached(10); !slices.Equal(got, []string{"wall_clock"}) {
		t.Fatalf("10: %v", got)
	}
	if got := ItemsReached(100); !slices.Equal(got, []string{"wall_clock", "picture_frame", "floor_lamp"}) {
		t.Fatalf("100: %v", got)
	}
	if got := ItemsReached(1340); len(got) != 12 || got[11] != "wind_chime" {
		t.Fatalf("1340: %v", got)
	}
}
