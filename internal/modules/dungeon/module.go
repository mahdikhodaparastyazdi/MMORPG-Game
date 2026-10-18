package dungeon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"dungeon-loot-backend/internal/modules/lootconfig"
	"dungeon-loot-backend/internal/modules/party"
	"dungeon-loot-backend/internal/shared/httpx"
	"github.com/google/uuid"
)

var (
	ErrRunNotFound = errors.New("dungeon run not found")
	ErrRunRule     = errors.New("dungeon start rule violation")
)

type Participant struct {
	PlayerID        string  `json:"player_id"`
	Username        string  `json:"username"`
	CharacterClass  string  `json:"character_class"`
	Contribution    float64 `json:"contribution"`
	EffectiveWeight float64 `json:"effective_weight"`
}

type GrantedReward struct {
	InstanceID     string    `json:"instance_id"`
	SettlementSlot int       `json:"settlement_slot"`
	ItemID         string    `json:"item_id"`
	ItemName       string    `json:"item_name"`
	ItemType       string    `json:"item_type"`
	Rarity         string    `json:"rarity"`
	InitialOwnerID string    `json:"initial_owner_id"`
	CurrentOwnerID string    `json:"current_owner_id"`
	AcquiredAt     time.Time `json:"acquired_at"`
}

type DungeonRun struct {
	ID                string          `json:"id"`
	PartyID           string          `json:"party_id"`
	DungeonID         string          `json:"dungeon_id"`
	Difficulty        int             `json:"difficulty"`
	LootConfigID      string          `json:"loot_config_id"`
	Status            string          `json:"status"`
	RNGSeed           *int64          `json:"rng_seed,omitempty"`
	ExchangeExpiresAt *time.Time      `json:"exchange_expires_at,omitempty"`
	StartedAt         time.Time       `json:"started_at"`
	SettledAt         *time.Time      `json:"settled_at,omitempty"`
	Participants      []Participant   `json:"participants"`
	Rewards           []GrantedReward `json:"rewards"`
	AuditRollDetails  json.RawMessage `json:"audit_roll_details,omitempty"`
}

type StartRunInput struct {
	PartyID    string `json:"party_id"`
	LeaderID   string `json:"leader_id"`
	DungeonID  string `json:"dungeon_id"`
	Difficulty int    `json:"difficulty"`
}

type Service struct {
	db            *sql.DB
	lootConfigSvc *lootconfig.Service
}

func NewService(db *sql.DB, lootConfigSvc *lootconfig.Service) *Service {
	return &Service{
		db:            db,
		lootConfigSvc: lootConfigSvc,
	}
}

func (s *Service) StartRun(ctx context.Context, input StartRunInput) (*DungeonRun, error) {
	if input.PartyID == "" || input.LeaderID == "" || input.DungeonID == "" || input.Difficulty <= 0 {
		return nil, fmt.Errorf("%w: party_id, leader_id, dungeon_id, and difficulty > 0 are required", ErrRunRule)
	}

	// 1. Verify active Loot Configuration exists for (dungeon_id, difficulty)
	activeCfg, err := s.lootConfigSvc.GetActiveConfig(ctx, input.DungeonID, input.Difficulty)
	if errors.Is(err, lootconfig.ErrConfigNotFound) {
		return nil, fmt.Errorf("%w: no active loot configuration found for dungeon %q difficulty %d", ErrRunRule, input.DungeonID, input.Difficulty)
	}
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 2. Lock Party row & verify Leader and Status
	var leaderID, status string
	err = tx.QueryRowContext(ctx, `
		SELECT leader_id, status FROM parties WHERE id = $1 FOR UPDATE
	`, input.PartyID).Scan(&leaderID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: party not found", ErrRunRule)
	}
	if err != nil {
		return nil, err
	}

	if leaderID != input.LeaderID {
		return nil, fmt.Errorf("%w: only party leader (%s) can start the dungeon", ErrRunRule, leaderID)
	}
	if status != "FORMING" {
		return nil, fmt.Errorf("%w: party cannot start a dungeon in status %s", ErrRunRule, status)
	}

	// 3. Load current party members and enforce 3..6 member count
	rows, err := tx.QueryContext(ctx, `
		SELECT player_id FROM party_members WHERE party_id = $1 ORDER BY joined_at ASC
	`, input.PartyID)
	if err != nil {
		return nil, err
	}
	var memberIDs []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return nil, err
		}
		memberIDs = append(memberIDs, pid)
	}
	rows.Close()

	if len(memberIDs) < party.MinPartySize || len(memberIDs) > party.MaxPartySize {
		return nil, fmt.Errorf(
			"%w: party must have between %d and %d members to start a dungeon (currently has %d)",
			ErrRunRule, party.MinPartySize, party.MaxPartySize, len(memberIDs),
		)
	}

	// 4. Create DungeonRun pinning the active loot_config_id
	runID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO dungeon_runs (id, party_id, dungeon_id, difficulty, loot_config_id, status)
		VALUES ($1, $2, $3, $4, $5, 'ACTIVE')
	`, runID, input.PartyID, input.DungeonID, input.Difficulty, activeCfg.ID)
	if err != nil {
		return nil, fmt.Errorf("insert dungeon_run: %w", err)
	}

	// 5. Lock in eligible Run Participants
	for _, pid := range memberIDs {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO run_participants (run_id, player_id, contribution, effective_weight)
			VALUES ($1, $2, 0.0, 0.0)
		`, runID, pid)
		if err != nil {
			return nil, fmt.Errorf("insert run_participant: %w", err)
		}
	}

	// 6. Update party status to IN_DUNGEON
	_, err = tx.ExecContext(ctx, `UPDATE parties SET status = 'IN_DUNGEON' WHERE id = $1`, input.PartyID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetRunByID(ctx, runID)
}

func (s *Service) GetRunByID(ctx context.Context, runID string) (*DungeonRun, error) {
	var run DungeonRun
	err := s.db.QueryRowContext(ctx, `
		SELECT id, party_id, dungeon_id, difficulty, loot_config_id, status,
		       rng_seed, exchange_expires_at, started_at, settled_at
		FROM dungeon_runs
		WHERE id = $1
	`, runID).Scan(
		&run.ID, &run.PartyID, &run.DungeonID, &run.Difficulty, &run.LootConfigID,
		&run.Status, &run.RNGSeed, &run.ExchangeExpiresAt, &run.StartedAt, &run.SettledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}

	pRows, err := s.db.QueryContext(ctx, `
		SELECT rp.player_id, p.username, p.character_class, rp.contribution, rp.effective_weight
		FROM run_participants rp
		JOIN players p ON p.id = rp.player_id
		WHERE rp.run_id = $1
		ORDER BY rp.player_id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()

	run.Participants = []Participant{}
	for pRows.Next() {
		var pt Participant
		if err := pRows.Scan(&pt.PlayerID, &pt.Username, &pt.CharacterClass, &pt.Contribution, &pt.EffectiveWeight); err != nil {
			return nil, err
		}
		run.Participants = append(run.Participants, pt)
	}

	rRows, err := s.db.QueryContext(ctx, `
		SELECT ii.id, ii.settlement_slot, ii.item_id, ic.name, ic.item_type, ic.rarity,
		       ii.initial_owner_id, ii.current_owner_id, ii.acquired_at
		FROM item_instances ii
		JOIN item_catalog ic ON ic.id = ii.item_id
		WHERE ii.origin_run_id = $1
		ORDER BY ii.settlement_slot ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rRows.Close()

	run.Rewards = []GrantedReward{}
	for rRows.Next() {
		var rw GrantedReward
		if err := rRows.Scan(
			&rw.InstanceID, &rw.SettlementSlot, &rw.ItemID, &rw.ItemName,
			&rw.ItemType, &rw.Rarity, &rw.InitialOwnerID, &rw.CurrentOwnerID, &rw.AcquiredAt,
		); err != nil {
			return nil, err
		}
		run.Rewards = append(run.Rewards, rw)
	}

	var auditJSON []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT roll_details FROM settlement_audit_logs WHERE run_id = $1
	`, runID).Scan(&auditJSON)
	if err == nil && len(auditJSON) > 0 {
		run.AuditRollDetails = auditJSON
	}

	return &run, nil
}

func (s *Service) ListRuns(ctx context.Context) ([]*DungeonRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM dungeon_runs ORDER BY started_at DESC LIMIT 20`)
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

	var out []*DungeonRun
	for _, id := range ids {
		r, err := s.GetRunByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs", h.handleListRuns)
	mux.HandleFunc("POST /api/runs/start", h.handleStartRun)
	mux.HandleFunc("GET /api/runs/{id}", h.handleGetRun)
}

func (h *Handler) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := h.svc.ListRuns(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, runs)
}

func (h *Handler) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var input StartRunInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	run, err := h.svc.StartRun(r.Context(), input)
	if errors.Is(err, ErrRunRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusCreated, run)
}

func (h *Handler) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.svc.GetRunByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrRunNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, run)
}