package player

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"dungeon-loot-backend/internal/shared/httpx"
)

var ErrPlayerNotFound = errors.New("player not found")

type Player struct {
	ID             string          `json:"id"`
	Username       string          `json:"username"`
	CharacterClass string          `json:"character_class"`
	GoldBalance    int64           `json:"gold_balance"`
	Inventory      []InventoryItem `json:"inventory,omitempty"`
}

type InventoryItem struct {
	InstanceID     string    `json:"instance_id"`
	ItemID         string    `json:"item_id"`
	Name           string    `json:"name"`
	ItemType       string    `json:"item_type"`
	Rarity         string    `json:"rarity"`
	OriginRunID    string    `json:"origin_run_id"`
	InitialOwnerID string    `json:"initial_owner_id"`
	AcquiredAt     time.Time `json:"acquired_at"`
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func (s *Service) ListPlayers(ctx context.Context) ([]Player, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, character_class, gold_balance
		FROM players
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var players []Player
	for rows.Next() {
		var p Player
		if err := rows.Scan(&p.ID, &p.Username, &p.CharacterClass, &p.GoldBalance); err != nil {
			return nil, err
		}
		players = append(players, p)
	}
	return players, rows.Err()
}

func (s *Service) GetPlayerWithInventory(ctx context.Context, playerID string) (*Player, error) {
	var p Player
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, character_class, gold_balance
		FROM players
		WHERE id = $1
	`, playerID).Scan(&p.ID, &p.Username, &p.CharacterClass, &p.GoldBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlayerNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ii.id, ii.item_id, ic.name, ic.item_type, ic.rarity,
		       ii.origin_run_id, ii.initial_owner_id, ii.acquired_at
		FROM item_instances ii
		JOIN item_catalog ic ON ic.id = ii.item_id
		WHERE ii.current_owner_id = $1
		ORDER BY ii.acquired_at DESC
	`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	p.Inventory = []InventoryItem{}
	for rows.Next() {
		var item InventoryItem
		if err := rows.Scan(
			&item.InstanceID, &item.ItemID, &item.Name, &item.ItemType, &item.Rarity,
			&item.OriginRunID, &item.InitialOwnerID, &item.AcquiredAt,
		); err != nil {
			return nil, err
		}
		p.Inventory = append(p.Inventory, item)
	}

	return &p, rows.Err()
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/players", h.handleListPlayers)
	mux.HandleFunc("GET /api/players/{id}", h.handleGetPlayer)
}

func (h *Handler) handleListPlayers(w http.ResponseWriter, r *http.Request) {
	players, err := h.svc.ListPlayers(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, players)
}

func (h *Handler) handleGetPlayer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := h.svc.GetPlayerWithInventory(r.Context(), id)
	if errors.Is(err, ErrPlayerNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}