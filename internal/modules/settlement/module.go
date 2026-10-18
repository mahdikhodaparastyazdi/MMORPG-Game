package settlement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"time"

	"dungeon-loot-backend/internal/modules/dungeon"
	"dungeon-loot-backend/internal/modules/lootconfig"
	"dungeon-loot-backend/internal/shared/httpx"
	"github.com/google/uuid"
)

var (
	ErrUnauthorizedServer = errors.New("unauthorized gameplay server token")
	ErrSettlementRule     = errors.New("settlement validation error")
)

// ContributionFloor guarantees that even the lowest contributor (e.g. contribution = 0)
// retains at least 20% of an equal share so their chance of receiving loot is never zero.
const ContributionFloor = 0.20

type CompletionInput struct {
	RunID         string             `json:"run_id"`
	DungeonID     string             `json:"dungeon_id"`
	Difficulty    int                `json:"difficulty"`
	Success       bool               `json:"success"`
	Contributions map[string]float64 `json:"contributions"`
	OptionalSeed  *int64             `json:"optional_seed,omitempty"`
}

type RollAuditEntry struct {
	Slot              int               `json:"slot"`
	Mode              string            `json:"mode"`
	TargetPlayerID    string            `json:"target_player_id,omitempty"`
	ContributionMult  float64           `json:"contribution_multiplier,omitempty"`
	RarityRoll        float64           `json:"rarity_roll"`
	SelectedRarity    lootconfig.Rarity `json:"selected_rarity"`
	ItemWeightRoll    int               `json:"item_weight_roll,omitempty"`
	TotalRarityWeight int               `json:"total_rarity_weight,omitempty"`
	SelectedItemID    string            `json:"selected_item_id,omitempty"`
	RecipientRoll     float64           `json:"recipient_roll,omitempty"`
	AwardedToPlayerID string            `json:"awarded_to_player_id,omitempty"`
}

type AuditDetails struct {
	ConfigID          string                 `json:"config_id"`
	ConfigVersion     int                    `json:"config_version"`
	AllocationMode    string                 `json:"allocation_mode"`
	ContributionMode  string                 `json:"contribution_mode"`
	RNGSeed           int64                  `json:"rng_seed"`
	EffectiveWeights  map[string]float64     `json:"effective_weights"`
	PersonalDropMulti map[string]float64     `json:"personal_drop_multipliers,omitempty"`
	Rolls             []RollAuditEntry       `json:"rolls"`
}

type PlannedDrop struct {
	Slot        int
	ItemID      string
	RecipientID string
}

type Service struct {
	db             *sql.DB
	lootConfigSvc  *lootconfig.Service
	dungeonSvc     *dungeon.Service
	exchangeWindow time.Duration
	serverSecret   string
}

func NewService(
	db *sql.DB,
	lootConfigSvc *lootconfig.Service,
	dungeonSvc *dungeon.Service,
	exchangeWindow time.Duration,
	serverSecret string,
) *Service {
	return &Service{
		db:             db,
		lootConfigSvc:  lootConfigSvc,
		dungeonSvc:     dungeonSvc,
		exchangeWindow: exchangeWindow,
		serverSecret:   serverSecret,
	}
}

// ComputeEffectiveWeights calculates normalized participant probabilities [0..1] summing to 1.0.
// In CONTRIBUTION_AWARE mode, it blends a guaranteed floor (ContributionFloor * 1/N) with the
// participant's relative share of total contribution so low contributors still have a valid chance.
func ComputeEffectiveWeights(participantIDs []string, contributions map[string]float64, mode lootconfig.ContributionMode) map[string]float64 {
	n := len(participantIDs)
	weights := make(map[string]float64, n)
	if n == 0 {
		return weights
	}

	equalShare := 1.0 / float64(n)
	if mode == lootconfig.ContributionIndependent {
		for _, pid := range participantIDs {
			weights[pid] = equalShare
		}
		return weights
	}

	totalContrib := 0.0
	for _, pid := range participantIDs {
		c := contributions[pid]
		if c > 0 {
			totalContrib += c
		}
	}

	// If everyone reported 0 contribution, fall back to equal shares
	if totalContrib <= 0 {
		for _, pid := range participantIDs {
			weights[pid] = equalShare
		}
		return weights
	}

	for _, pid := range participantIDs {
		c := math.Max(0, contributions[pid])
		contribShare := c / totalContrib
		weights[pid] = (ContributionFloor * equalShare) + ((1.0 - ContributionFloor) * contribShare)
	}
	return weights
}

// ComputePersonalMultipliers calculates a drop chance multiplier per participant for PERSONAL_LOOT.
// Independent mode gives everyone 1.0x. Contribution-Aware scales between [0.5x, 1.5x] based on
// how the player's effective weight compares to an equal share (1/N).
func ComputePersonalMultipliers(participantIDs []string, effectiveWeights map[string]float64, mode lootconfig.ContributionMode) map[string]float64 {
	out := make(map[string]float64, len(participantIDs))
	n := len(participantIDs)
	if n == 0 {
		return out
	}
	equalShare := 1.0 / float64(n)

	for _, pid := range participantIDs {
		if mode == lootconfig.ContributionIndependent || equalShare == 0 {
			out[pid] = 1.0
			continue
		}
		ratio := effectiveWeights[pid] / equalShare
		// Clamp between 0.50 (guaranteed floor chance) and 1.50 (high-contributor boost)
		mult := math.Max(0.50, math.Min(1.50, ratio))
		out[pid] = mult
	}
	return out
}

// ExecuteLootRolls is a pure, deterministic function given an RNG seed and LootConfig.
func ExecuteLootRolls(
	seed int64,
	cfg *lootconfig.LootConfig,
	participantIDs []string,
	contributions map[string]float64,
) ([]PlannedDrop, AuditDetails) {
	// Sort participant IDs for 100% deterministic RNG iteration order
	sortedParticipants := append([]string(nil), participantIDs...)
	sort.Strings(sortedParticipants)

	rng := rand.New(rand.NewSource(seed))
	effWeights := ComputeEffectiveWeights(sortedParticipants, contributions, cfg.ContributionMode)
	personalMults := ComputePersonalMultipliers(sortedParticipants, effWeights, cfg.ContributionMode)

	// Group eligible items by Rarity in deterministic order
	itemsByRarity := make(map[lootconfig.Rarity][]lootconfig.EligibleItem)
	for _, it := range cfg.EligibleItems {
		itemsByRarity[it.Rarity] = append(itemsByRarity[it.Rarity], it)
	}

	// Deterministic Rarity evaluation order (Legendary -> Epic -> Rare -> Common)
	orderedRarities := []lootconfig.Rarity{
		lootconfig.RarityLegendary,
		lootconfig.RarityEpic,
		lootconfig.RarityRare,
		lootconfig.RarityCommon,
	}

	audit := AuditDetails{
		ConfigID:          cfg.ID,
		ConfigVersion:     cfg.Version,
		AllocationMode:    string(cfg.AllocationMode),
		ContributionMode:  string(cfg.ContributionMode),
		RNGSeed:           seed,
		EffectiveWeights:  effWeights,
		PersonalDropMulti: personalMults,
		Rolls:             []RollAuditEntry{},
	}

	var drops []PlannedDrop
	slotCounter := 1

	// Helper for Stage 1 (Rarity Roll) + Stage 2 (Intra-Rarity Weighted Item Roll)
	rollRarityAndItem := func(probMultiplier float64) (lootconfig.Rarity, string, float64, int, int) {
		rRoll := rng.Float64()
		var chosenRarity lootconfig.Rarity
		cumulative := 0.0

		for _, rarity := range orderedRarities {
			baseProb := cfg.RarityChances[rarity]
			if baseProb <= 0 {
				continue
			}
			effectiveProb := baseProb * probMultiplier
			cumulative += effectiveProb
			if rRoll < cumulative {
				chosenRarity = rarity
				break
			}
		}

		if chosenRarity == "" {
			return "NONE", "", rRoll, 0, 0
		}

		pool := itemsByRarity[chosenRarity]
		totalWeight := 0
		for _, item := range pool {
			totalWeight += item.Weight
		}
		if totalWeight <= 0 {
			return chosenRarity, "", rRoll, 0, 0
		}

		wRoll := rng.Intn(totalWeight)
		runningWeight := 0
		var chosenItemID string
		for _, item := range pool {
			runningWeight += item.Weight
			if wRoll < runningWeight {
				chosenItemID = item.ItemID
				break
			}
		}

		return chosenRarity, chosenItemID, rRoll, wRoll, totalWeight
	}

	if cfg.AllocationMode == lootconfig.AllocationPartyPool {
		// PARTY_POOL: Roll N shared items for the run and allocate each item among participants
		for i := 0; i < cfg.PartyPoolDropCount; i++ {
			rarity, itemID, rRoll, wRoll, totalW := rollRarityAndItem(1.0)
			entry := RollAuditEntry{
				Slot:              slotCounter,
				Mode:              string(lootconfig.AllocationPartyPool),
				RarityRoll:        rRoll,
				SelectedRarity:    rarity,
				ItemWeightRoll:    wRoll,
				TotalRarityWeight: totalW,
				SelectedItemID:    itemID,
			}

			if itemID != "" {
				recipRoll := rng.Float64()
				cumWeight := 0.0
				winnerID := sortedParticipants[len(sortedParticipants)-1]
				for _, pid := range sortedParticipants {
					cumWeight += effWeights[pid]
					if recipRoll < cumWeight {
						winnerID = pid
						break
					}
				}
				entry.RecipientRoll = recipRoll
				entry.AwardedToPlayerID = winnerID

				drops = append(drops, PlannedDrop{
					Slot:        slotCounter,
					ItemID:      itemID,
					RecipientID: winnerID,
				})
			}

			audit.Rolls = append(audit.Rolls, entry)
			slotCounter++
		}
	} else {
		// PERSONAL_LOOT: Roll independently for each eligible participant
		for _, pid := range sortedParticipants {
			mult := personalMults[pid]
			rarity, itemID, rRoll, wRoll, totalW := rollRarityAndItem(mult)
			entry := RollAuditEntry{
				Slot:              slotCounter,
				Mode:              string(lootconfig.AllocationPersonalLoot),
				TargetPlayerID:    pid,
				ContributionMult:  mult,
				RarityRoll:        rRoll,
				SelectedRarity:    rarity,
				ItemWeightRoll:    wRoll,
				TotalRarityWeight: totalW,
				SelectedItemID:    itemID,
			}

			if itemID != "" {
				entry.AwardedToPlayerID = pid
				drops = append(drops, PlannedDrop{
					Slot:        slotCounter,
					ItemID:      itemID,
					RecipientID: pid,
				})
			}

			audit.Rolls = append(audit.Rolls, entry)
			slotCounter++
		}
	}

	return drops, audit
}

// SettleRun processes the Gameplay Server's Dungeon Completion submission idempotently.
func (s *Service) SettleRun(ctx context.Context, input CompletionInput) (*dungeon.DungeonRun, error) {
	if input.RunID == "" {
		return nil, fmt.Errorf("%w: run_id is required", ErrSettlementRule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. Lock the DungeonRun row with FOR UPDATE to serialize concurrent retries
	var partyID, dbDungeonID, lootConfigID, status string
	var dbDifficulty int
	err = tx.QueryRowContext(ctx, `
		SELECT party_id, dungeon_id, difficulty, loot_config_id, status
		FROM dungeon_runs
		WHERE id = $1
		FOR UPDATE
	`, input.RunID).Scan(&partyID, &dbDungeonID, &dbDifficulty, &lootConfigID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, dungeon.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}

	// 2. IDEMPOTENCY CHECK: If already settled, commit/release lock and return the existing result!
	if status == "SETTLED_SUCCESS" || status == "SETTLED_FAILED" {
		_ = tx.Rollback()
		return s.dungeonSvc.GetRunByID(ctx, input.RunID)
	}

	// Validate dungeon_id and difficulty if provided by Gameplay Server
	if input.DungeonID != "" && input.DungeonID != dbDungeonID {
		return nil, fmt.Errorf("%w: dungeon_id mismatch (expected %s, got %s)", ErrSettlementRule, dbDungeonID, input.DungeonID)
	}
	if input.Difficulty != 0 && input.Difficulty != dbDifficulty {
		return nil, fmt.Errorf("%w: difficulty mismatch (expected %d, got %d)", ErrSettlementRule, dbDifficulty, input.Difficulty)
	}

	// 3. Load locked Run Participants
	rows, err := tx.QueryContext(ctx, `
		SELECT player_id FROM run_participants WHERE run_id = $1 ORDER BY player_id ASC
	`, input.RunID)
	if err != nil {
		return nil, err
	}
	var participantIDs []string
	participantSet := make(map[string]bool)
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return nil, err
		}
		participantIDs = append(participantIDs, pid)
		participantSet[pid] = true
	}
	rows.Close()

	// Verify any reported contributions belong only to eligible participants of this run
	for pid, val := range input.Contributions {
		if !participantSet[pid] {
			return nil, fmt.Errorf("%w: player %q in contributions is not an eligible participant of this run", ErrSettlementRule, pid)
		}
		if val < 0 || math.IsNaN(val) {
			return nil, fmt.Errorf("%w: contribution for player %q cannot be negative", ErrSettlementRule, pid)
		}
	}

	// 4. Load the Pinned Immutable LootConfig Version
	pinnedCfg, err := s.lootConfigSvc.GetConfigByID(ctx, lootConfigID)
	if err != nil {
		return nil, fmt.Errorf("load pinned loot config %s: %w", lootConfigID, err)
	}

	now := time.Now().UTC()
	var seed int64
	if input.OptionalSeed != nil {
		seed = *input.OptionalSeed
	} else {
		seed = now.UnixNano()
	}

	effWeights := ComputeEffectiveWeights(participantIDs, input.Contributions, pinnedCfg.ContributionMode)

	// Update participant contribution & effective_weight records
	for _, pid := range participantIDs {
		c := input.Contributions[pid]
		w := effWeights[pid]
		_, err = tx.ExecContext(ctx, `
			UPDATE run_participants
			SET contribution = $1, effective_weight = $2
			WHERE run_id = $3 AND player_id = $4
		`, c, w, input.RunID, pid)
		if err != nil {
			return nil, err
		}
	}

	// 5. Handle Failed Dungeon Run (no rewards, no exchange window)
	if !input.Success {
		_, err = tx.ExecContext(ctx, `
			UPDATE dungeon_runs
			SET status = 'SETTLED_FAILED', rng_seed = $1, settled_at = $2
			WHERE id = $3
		`, seed, now, input.RunID)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE parties SET status = 'COMPLETED' WHERE id = $1`, partyID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return s.dungeonSvc.GetRunByID(ctx, input.RunID)
	}

	// 6. Execute Deterministic Loot Rolls for Successful Run
	drops, auditDetails := ExecuteLootRolls(seed, pinnedCfg, participantIDs, input.Contributions)

	for _, drop := range drops {
		instanceID := uuid.New().String()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO item_instances (id, item_id, origin_run_id, settlement_slot, initial_owner_id, current_owner_id, acquired_at)
			VALUES ($1, $2, $3, $4, $5, $5, $6)
		`, instanceID, drop.ItemID, input.RunID, drop.Slot, drop.RecipientID, now)
		if err != nil {
			return nil, fmt.Errorf("insert item_instance: %w", err)
		}

		// Record immutable provenance in economy_ledger
		ledgerID := uuid.New().String()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO economy_ledger (id, run_id, event_type, item_instance_id, to_player_id, gold_transferred, created_at)
			VALUES ($1, $2, 'DUNGEON_REWARD_GRANT', $3, $4, 0, $5)
		`, ledgerID, input.RunID, instanceID, drop.RecipientID, now)
		if err != nil {
			return nil, fmt.Errorf("insert reward ledger: %w", err)
		}
	}

	// 7. Persist Audit Log for Operator/CS Investigation
	auditBytes, err := json.Marshal(auditDetails)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO settlement_audit_logs (run_id, loot_config_id, rng_seed, roll_details, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, input.RunID, pinnedCfg.ID, seed, auditBytes, now)
	if err != nil {
		return nil, fmt.Errorf("insert settlement_audit_log: %w", err)
	}

	// 8. Mark Run SETTLED_SUCCESS and Open Post-Dungeon Exchange Window
	expiresAt := now.Add(s.exchangeWindow)
	_, err = tx.ExecContext(ctx, `
		UPDATE dungeon_runs
		SET status = 'SETTLED_SUCCESS', rng_seed = $1, exchange_expires_at = $2, settled_at = $3
		WHERE id = $4
	`, seed, expiresAt, now, input.RunID)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `UPDATE parties SET status = 'COMPLETED' WHERE id = $1`, partyID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.dungeonSvc.GetRunByID(ctx, input.RunID)
}

type Handler struct {
	svc          *Service
	serverSecret string
}

func NewHandler(svc *Service, serverSecret string) *Handler {
	return &Handler{
		svc:          svc,
		serverSecret: serverSecret,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Authoritative Gameplay Server Endpoint (requires X-Server-Token header)
	mux.HandleFunc("POST /api/internal/gameplay/runs/{id}/complete", h.handleServerComplete)
	// Prototype Simulation Endpoint for UI convenience (simulates Gameplay Server call)
	mux.HandleFunc("POST /api/runs/{id}/simulate-complete", h.handleSimulateComplete)
}

func (h *Handler) handleServerComplete(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Server-Token")
	if token == "" || token != h.serverSecret {
		httpx.Error(w, http.StatusUnauthorized, ErrUnauthorizedServer.Error())
		return
	}
	h.processCompletion(w, r)
}

func (h *Handler) handleSimulateComplete(w http.ResponseWriter, r *http.Request) {
	// In the prototype UI, this endpoint acts as the Gameplay Server simulator
	// submitting the completion result across the internal trust boundary.
	h.processCompletion(w, r)
}

func (h *Handler) processCompletion(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	var input CompletionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
		return
	}
	input.RunID = runID

	run, err := h.svc.SettleRun(r.Context(), input)
	if errors.Is(err, dungeon.ErrRunNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrSettlementRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, run)
}