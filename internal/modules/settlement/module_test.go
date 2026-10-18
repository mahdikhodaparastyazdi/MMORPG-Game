package settlement

import (
	"math"
	"testing"

	"dungeon-loot-backend/internal/modules/lootconfig"
)

func TestComputeEffectiveWeights_ZeroContributorRetainsFloorChance(t *testing.T) {
	participants := []string{"player-a", "player-b", "player-c", "player-d"}
	contributions := map[string]float64{
		"player-a": 1000,
		"player-b": 500,
		"player-c": 200,
		"player-d": 0, // Zero contribution!
	}

	weights := ComputeEffectiveWeights(participants, contributions, lootconfig.ContributionAware)

	if weights["player-d"] <= 0 {
		t.Fatalf("expected zero-contribution participant to still have > 0 weight, got %f", weights["player-d"])
	}
	if weights["player-a"] <= weights["player-b"] || weights["player-b"] <= weights["player-c"] || weights["player-c"] <= weights["player-d"] {
		t.Fatalf("expected strictly increasing weights with contribution, got: %+v", weights)
	}

	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("expected weights to sum to 1.0, got %f", sum)
	}
}

func TestExecuteLootRolls_RarityProbabilityRemainsConstantWhenItemWeightsChange(t *testing.T) {
	participants := []string{"player-a", "player-b", "player-c"}
	contribs := map[string]float64{"player-a": 100, "player-b": 100, "player-c": 100}

	baseConfig := func(bladeWeight, mantleWeight int) *lootconfig.LootConfig {
		return &lootconfig.LootConfig{
			ID:                 "cfg-1",
			Version:            1,
			AllocationMode:     lootconfig.AllocationPartyPool,
			ContributionMode:   lootconfig.ContributionIndependent,
			PartyPoolDropCount: 1000,
			RarityChances: map[lootconfig.Rarity]float64{
				lootconfig.RarityCommon: 0.70,
				lootconfig.RarityEpic:   0.30,
			},
			EligibleItems: []lootconfig.EligibleItem{
				{ItemID: "item-iron-sword", Rarity: lootconfig.RarityCommon, Weight: 100},
				{ItemID: "item-shadow-blade", Rarity: lootconfig.RarityEpic, Weight: bladeWeight},
				{ItemID: "item-void-mantle", Rarity: lootconfig.RarityEpic, Weight: mantleWeight},
			},
		}
	}

	// Run 1: Epic weights are 50 / 50
	_, audit1 := ExecuteLootRolls(42, baseConfig(50, 50), participants, contribs)
	// Run 2: Epic weights are 900 / 100 (overall Epic probability is still 0.30!)
	_, audit2 := ExecuteLootRolls(42, baseConfig(900, 100), participants, contribs)

	epicCount1, epicCount2 := 0, 0
	for i := range audit1.Rolls {
		if audit1.Rolls[i].SelectedRarity == lootconfig.RarityEpic {
			epicCount1++
		}
		if audit2.Rolls[i].SelectedRarity == lootconfig.RarityEpic {
			epicCount2++
		}
	}

	// Because Stage 1 (Rarity roll) is decoupled from Stage 2 (Item Weight roll),
	// with the same seed, the exact same number of EPIC drops must occur!
	if epicCount1 != epicCount2 {
		t.Fatalf("expected identical EPIC rarity drop count across weight changes, got %d vs %d", epicCount1, epicCount2)
	}
}