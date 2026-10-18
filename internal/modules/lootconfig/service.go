package lootconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

var (
	ErrConfigNotFound = errors.New("loot configuration not found")
	ErrInvalidConfig  = errors.New("invalid loot configuration")
)

type Rarity string

const (
	RarityCommon    Rarity = "COMMON"
	RarityRare      Rarity = "RARE"
	RarityEpic      Rarity = "EPIC"
	RarityLegendary Rarity = "LEGENDARY"
)

var ValidRarities = map[Rarity]bool{
	RarityCommon:    true,
	RarityRare:      true,
	RarityEpic:      true,
	RarityLegendary: true,
}

type AllocationMode string

const (
	AllocationPartyPool    AllocationMode = "PARTY_POOL"
	AllocationPersonalLoot AllocationMode = "PERSONAL_LOOT"
)

type ContributionMode string

const (
	ContributionIndependent ContributionMode = "INDEPENDENT"
	ContributionAware       ContributionMode = "CONTRIBUTION_AWARE"
)

type CatalogItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	ItemType string    `json:"item_type"`
	Rarity   Rarity    `json:"rarity"`
	Created  time.Time `json:"created_at"`
}

type EligibleItem struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name,omitempty"`
	ItemType string `json:"item_type,omitempty"`
	Rarity   Rarity `json:"rarity,omitempty"`
	Weight   int    `json:"weight"`
	// RelativeChanceWithinRarity is computed dynamically for UI/Operator visibility
	RelativeChanceWithinRarity float64 `json:"relative_chance_within_rarity,omitempty"`
}

type LootConfig struct {
	ID                 string             `json:"id"`
	DungeonID          string             `json:"dungeon_id"`
	Difficulty         int                `json:"difficulty"`
	Version            int                `json:"version"`
	AllocationMode     AllocationMode     `json:"allocation_mode"`
	ContributionMode   ContributionMode   `json:"contribution_mode"`
	PartyPoolDropCount int                `json:"party_pool_drop_count"`
	IsActive           bool               `json:"is_active"`
	RarityChances      map[Rarity]float64 `json:"rarity_chances"`
	EligibleItems      []EligibleItem     `json:"eligible_items"`
	CreatedAt          time.Time          `json:"created_at"`
}

type UpsertConfigInput struct {
	DungeonID          string             `json:"dungeon_id"`
	Difficulty         int                `json:"difficulty"`
	AllocationMode     AllocationMode     `json:"allocation_mode"`
	ContributionMode   ContributionMode   `json:"contribution_mode"`
	PartyPoolDropCount int                `json:"party_pool_drop_count"`
	RarityChances      map[Rarity]float64 `json:"rarity_chances"`
	EligibleItems      []EligibleItem     `json:"eligible_items"`
}

// ValidateRules enforces all domain invariants on a candidate Loot Configuration
// against the authoritative Item Catalog.
func ValidateRules(input UpsertConfigInput, catalog map[string]CatalogItem) error {
	if input.DungeonID == "" {
		return fmt.Errorf("%w: dungeon_id is required", ErrInvalidConfig)
	}
	if input.Difficulty <= 0 {
		return fmt.Errorf("%w: difficulty must be > 0", ErrInvalidConfig)
	}
	if input.AllocationMode != AllocationPartyPool && input.AllocationMode != AllocationPersonalLoot {
		return fmt.Errorf("%w: allocation_mode must be PARTY_POOL or PERSONAL_LOOT", ErrInvalidConfig)
	}
	if input.ContributionMode != ContributionIndependent && input.ContributionMode != ContributionAware {
		return fmt.Errorf("%w: contribution_mode must be INDEPENDENT or CONTRIBUTION_AWARE", ErrInvalidConfig)
	}
	if input.PartyPoolDropCount <= 0 {
		return fmt.Errorf("%w: party_pool_drop_count must be > 0", ErrInvalidConfig)
	}
	if len(input.RarityChances) == 0 {
		return fmt.Errorf("%w: rarity_chances cannot be empty", ErrInvalidConfig)
	}

	totalProb := 0.0
	hasPositiveProb := false
	for rarity, prob := range input.RarityChances {
		if !ValidRarities[rarity] {
			return fmt.Errorf("%w: unknown rarity tier %q", ErrInvalidConfig, rarity)
		}
		if prob < 0.0 || prob > 1.0 || math.IsNaN(prob) {
			return fmt.Errorf("%w: probability for rarity %s must be between 0.0 and 1.0", ErrInvalidConfig, rarity)
		}
		if prob > 0 {
			hasPositiveProb = true
		}
		totalProb += prob
	}
	if !hasPositiveProb {
		return fmt.Errorf("%w: at least one rarity tier must have drop_probability > 0", ErrInvalidConfig)
	}
	if totalProb > 1.000001 {
		return fmt.Errorf("%w: sum of rarity drop probabilities (%.4f) cannot exceed 1.0", ErrInvalidConfig, totalProb)
	}

	// Track eligible item count & weight sum per rarity (derived from Item Catalog!)
	seenItems := make(map[string]bool)
	rarityWeightSum := make(map[Rarity]int)

	for _, item := range input.EligibleItems {
		if item.ItemID == "" {
			return fmt.Errorf("%w: eligible item has empty item_id", ErrInvalidConfig)
		}
		if seenItems[item.ItemID] {
			return fmt.Errorf("%w: duplicate item_id %q in eligible_items", ErrInvalidConfig, item.ItemID)
		}
		seenItems[item.ItemID] = true

		if item.Weight <= 0 {
			return fmt.Errorf("%w: weight for item %q must be > 0", ErrInvalidConfig, item.ItemID)
		}

		catItem, exists := catalog[item.ItemID]
		if !exists {
			return fmt.Errorf("%w: item_id %q does not exist in item_catalog", ErrInvalidConfig, item.ItemID)
		}

		rarityWeightSum[catItem.Rarity] += item.Weight
	}

	// CRITICAL INVARIANT: If a Rarity has Drop Probability > 0, there MUST be at least
	// one eligible item in the catalog for that Rarity with weight > 0.
	for rarity, prob := range input.RarityChances {
		if prob > 0.0 && rarityWeightSum[rarity] == 0 {
			return fmt.Errorf(
				"%w: rarity %s has drop_probability %.4f (> 0), but zero eligible items of rarity %s are configured",
				ErrInvalidConfig, rarity, prob, rarity,
			)
		}
	}

	return nil
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// ListCatalog returns all predefined items in the Item Catalog.
func (s *Service) ListCatalog(ctx context.Context) ([]CatalogItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, item_type, rarity, created_at
		FROM item_catalog
		ORDER BY rarity, id
	`)
	if err != nil {
		return nil, fmt.Errorf("query item_catalog: %w", err)
	}
	defer rows.Close()

	var items []CatalogItem
	for rows.Next() {
		var it CatalogItem
		if err := rows.Scan(&it.ID, &it.Name, &it.ItemType, &it.Rarity, &it.Created); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Service) getCatalogMap(ctx context.Context, tx *sql.Tx) (map[string]CatalogItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, item_type, rarity, created_at FROM item_catalog`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]CatalogItem)
	for rows.Next() {
		var it CatalogItem
		if err := rows.Scan(&it.ID, &it.Name, &it.ItemType, &it.Rarity, &it.Created); err != nil {
			return nil, err
		}
		m[it.ID] = it
	}
	return m, rows.Err()
}

// UpsertActiveConfig validates the configuration and creates a new immutable version,
// deactivating the previous active version for that (dungeon_id, difficulty) so historical runs remain untouched.
func (s *Service) UpsertActiveConfig(ctx context.Context, input UpsertConfigInput) (*LootConfig, error) {
	if input.PartyPoolDropCount == 0 {
		input.PartyPoolDropCount = 4
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	catalogMap, err := s.getCatalogMap(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}

	if err := ValidateRules(input, catalogMap); err != nil {
		return nil, err
	}

	// Find current max version for (dungeon_id, difficulty)
	var maxVersion int
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0)
		FROM loot_configs
		WHERE dungeon_id = $1 AND difficulty = $2
	`, input.DungeonID, input.Difficulty).Scan(&maxVersion)
	if err != nil {
		return nil, fmt.Errorf("check max version: %w", err)
	}

	// Mark existing versions inactive
	_, err = tx.ExecContext(ctx, `
		UPDATE loot_configs
		SET is_active = FALSE
		WHERE dungeon_id = $1 AND difficulty = $2 AND is_active = TRUE
	`, input.DungeonID, input.Difficulty)
	if err != nil {
		return nil, fmt.Errorf("deactivate old configs: %w", err)
	}

	newID := uuid.New().String()
	newVersion := maxVersion + 1

	_, err = tx.ExecContext(ctx, `
		INSERT INTO loot_configs (id, dungeon_id, difficulty, version, allocation_mode, contribution_mode, party_pool_drop_count, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE)
	`, newID, input.DungeonID, input.Difficulty, newVersion, input.AllocationMode, input.ContributionMode, input.PartyPoolDropCount)
	if err != nil {
		return nil, fmt.Errorf("insert loot_config: %w", err)
	}

	// Insert all 4 rarities (defaulting omitted ones to 0.0)
	for rarity := range ValidRarities {
		prob := input.RarityChances[rarity]
		_, err = tx.ExecContext(ctx, `
			INSERT INTO loot_config_rarities (loot_config_id, rarity, drop_probability)
			VALUES ($1, $2, $3)
		`, newID, rarity, prob)
		if err != nil {
			return nil, fmt.Errorf("insert rarity probability: %w", err)
		}
	}

	for _, item := range input.EligibleItems {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO loot_config_items (loot_config_id, item_id, weight)
			VALUES ($1, $2, $3)
		`, newID, item.ItemID, item.Weight)
		if err != nil {
			return nil, fmt.Errorf("insert eligible item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit config transaction: %w", err)
	}

	return s.GetConfigByID(ctx, newID)
}

// GetActiveConfig fetches the currently active configuration for a (dungeon_id, difficulty).
func (s *Service) GetActiveConfig(ctx context.Context, dungeonID string, difficulty int) (*LootConfig, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM loot_configs
		WHERE dungeon_id = $1 AND difficulty = $2 AND is_active = TRUE
		ORDER BY version DESC LIMIT 1
	`, dungeonID, difficulty).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetConfigByID(ctx, id)
}

// GetConfigByID loads a specific immutable LootConfig version (used both by active lookups and pinned Dungeon Runs).
func (s *Service) GetConfigByID(ctx context.Context, configID string) (*LootConfig, error) {
	cfg := &LootConfig{
		RarityChances: make(map[Rarity]float64),
		EligibleItems: []EligibleItem{},
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT id, dungeon_id, difficulty, version, allocation_mode, contribution_mode, party_pool_drop_count, is_active, created_at
		FROM loot_configs
		WHERE id = $1
	`, configID).Scan(
		&cfg.ID, &cfg.DungeonID, &cfg.Difficulty, &cfg.Version,
		&cfg.AllocationMode, &cfg.ContributionMode, &cfg.PartyPoolDropCount,
		&cfg.IsActive, &cfg.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigNotFound
	}
	if err != nil {
		return nil, err
	}

	rRows, err := s.db.QueryContext(ctx, `
		SELECT rarity, drop_probability
		FROM loot_config_rarities
		WHERE loot_config_id = $1
	`, configID)
	if err != nil {
		return nil, err
	}
	defer rRows.Close()

	for rRows.Next() {
		var r Rarity
		var p float64
		if err := rRows.Scan(&r, &p); err != nil {
			return nil, err
		}
		cfg.RarityChances[r] = p
	}

	iRows, err := s.db.QueryContext(ctx, `
		SELECT lci.item_id, ic.name, ic.item_type, ic.rarity, lci.weight
		FROM loot_config_items lci
		JOIN item_catalog ic ON ic.id = lci.item_id
		WHERE lci.loot_config_id = $1
		ORDER BY ic.rarity, lci.item_id
	`, configID)
	if err != nil {
		return nil, err
	}
	defer iRows.Close()

	rarityTotalWeight := make(map[Rarity]int)
	for iRows.Next() {
		var item EligibleItem
		if err := iRows.Scan(&item.ItemID, &item.Name, &item.ItemType, &item.Rarity, &item.Weight); err != nil {
			return nil, err
		}
		rarityTotalWeight[item.Rarity] += item.Weight
		cfg.EligibleItems = append(cfg.EligibleItems, item)
	}

	// Calculate relative intra-rarity probability for each eligible item
	for i := range cfg.EligibleItems {
		r := cfg.EligibleItems[i].Rarity
		if total := rarityTotalWeight[r]; total > 0 {
			cfg.EligibleItems[i].RelativeChanceWithinRarity = float64(cfg.EligibleItems[i].Weight) / float64(total)
		}
	}

	return cfg, nil
}

// ListActiveConfigs returns all active dungeon configurations.
func (s *Service) ListActiveConfigs(ctx context.Context) ([]*LootConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM loot_configs
		WHERE is_active = TRUE
		ORDER BY dungeon_id, difficulty
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	var result []*LootConfig
	for _, id := range ids {
		cfg, err := s.GetConfigByID(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, cfg)
	}
	return result, nil
}
