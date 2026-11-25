package service

import (
	"slices"
	"time"
)

// Check-in numbers (B6b spec 5.1, 5.2).
const (
	CheckinEnergy   = 10
	FullHouseBonus  = 20
	FullHouseWindow = 24 * time.Hour
)

// DecorItem is a decoration and the room energy that unlocks it.
type DecorItem struct {
	ID        string
	Threshold int
}

// decorCatalog is ordered by threshold. Names and drawings live in the client.
var decorCatalog = []DecorItem{
	{"wall_clock", 10},
	{"picture_frame", 40},
	{"floor_lamp", 80},
	{"string_lights", 140},
	{"potted_tree", 220},
	{"wall_map", 320},
	{"woven_basket", 440},
	{"bookshelf", 580},
	{"mailbox", 740},
	{"yarn_ball", 920},
	{"record_player", 1120},
	{"wind_chime", 1340},
}

// DecorCatalog returns a copy of the catalog, lowest threshold first.
func DecorCatalog() []DecorItem { return slices.Clone(decorCatalog) }

// NextThreshold is the lowest threshold above total, nil once all are reached.
func NextThreshold(total int) *int {
	for _, d := range decorCatalog {
		if d.Threshold > total {
			n := d.Threshold
			return &n
		}
	}
	return nil
}

// ItemsReached lists the items total energy unlocks, in catalog order; never nil.
func ItemsReached(total int) []string {
	out := []string{}
	for _, d := range decorCatalog {
		if d.Threshold <= total {
			out = append(out, d.ID)
		}
	}
	return out
}
