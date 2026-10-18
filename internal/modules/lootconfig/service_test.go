package lootconfig

import (
	"strings"
	"testing"
)

func sampleCatalog() map[string]CatalogItem {
	return map[string]CatalogItem{
		"item-iron-sword":   {ID: "item-iron-sword", Name: "Iron Sword", Rarity: RarityCommon},
		"item-shadow-blade": {ID: "item-shadow-blade", Name: "Shadow Blade", Rarity: RarityEpic},
		"item-void-mantle":  {ID: "item-void-mantle", Name: "Void Mantle", Rarity: RarityEpic},
	}
}

func TestValidateRules_EmptyEligiblePoolForPositiveRarityFails(t *testing.T) {
	catalog := sampleCatalog()

	input := UpsertConfigInput{
		DungeonID:          "dungeon-x",
		Difficulty:         18,
		AllocationMode:     AllocationPartyPool,
		ContributionMode:   ContributionAware,
		PartyPoolDropCount: 4,
		RarityChances: map[Rarity]float64{
			RarityCommon:    0.70,
			RarityEpic:      0.20,
			RarityLegendary: 0.10, // > 0 probability, but no LEGENDARY item in EligibleItems!
		},
		EligibleItems: []EligibleItem{
			{ItemID: "item-iron-sword", Weight: 100},
			{ItemID: "item-shadow-blade", Weight: 50},
		},
	}

	err := ValidateRules(input, catalog)
	if err == nil {
		t.Fatalf("expected error when LEGENDARY has probability > 0 but no eligible items, got nil")
	}
	if !strings.Contains(err.Error(), "LEGENDARY") {
		t.Fatalf("expected error message to mention LEGENDARY rarity, got: %v", err)
	}
}

func TestValidateRules_ValidConfigurationPasses(t *testing.T) {
	catalog := sampleCatalog()

	input := UpsertConfigInput{
		DungeonID:          "dungeon-x",
		Difficulty:         18,
		AllocationMode:     AllocationPersonalLoot,
		ContributionMode:   ContributionIndependent,
		PartyPoolDropCount: 4,
		RarityChances: map[Rarity]float64{
			RarityCommon:    0.80,
			RarityEpic:      0.20,
			RarityLegendary: 0.00, // 0 probability is valid with 0 items
		},
		EligibleItems: []EligibleItem{
			{ItemID: "item-iron-sword", Weight: 100},
			{ItemID: "item-shadow-blade", Weight: 75},
			{ItemID: "item-void-mantle", Weight: 25},
		},
	}

	if err := ValidateRules(input, catalog); err != nil {
		t.Fatalf("expected valid config to pass, got error: %v", err)
	}
}