package party

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dungeon-loot-backend/internal/shared/httpx"

	"github.com/google/uuid"
)

var (
	ErrPartyNotFound = errors.New("party not found")
	ErrPartyRule     = errors.New("party rule violation")
)

const (
	MinPartySize = 3
	MaxPartySize = 6
)

type Member struct {
	PlayerID       string    `json:"player_id"`
	Username       string    `json:"username"`
	CharacterClass string    `json:"character_class"`
	JoinedAt       time.Time `json:"joined_at"`
}

type Party struct {
	ID        string    `json:"id"`
	JoinCode  string    `json:"join_code"`
	LeaderID  string    `json:"leader_id"`
	Status    string    `json:"status"`
	Members   []Member  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func generateJoinCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func (s *Service) CreateParty(ctx context.Context, leaderID string) (*Party, error) {
	leaderID = strings.TrimSpace(leaderID)
	if leaderID == "" {
		return nil, fmt.Errorf("%w: leader_id is required", ErrPartyRule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM players WHERE id = $1)`, leaderID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: player %q does not exist", ErrPartyRule, leaderID)
	}

	partyID := uuid.New().String()
	joinCode := generateJoinCode()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO parties (id, join_code, leader_id, status)
		VALUES ($1, $2, $3, 'FORMING')
	`, partyID, joinCode, leaderID)
	if err != nil {
		return nil, fmt.Errorf("insert party: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO party_members (party_id, player_id)
		VALUES ($1, $2)
	`, partyID, leaderID)
	if err != nil {
		return nil, fmt.Errorf("insert leader into party_members: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetPartyByID(ctx, partyID)
}

func (s *Service) JoinPartyByCode(ctx context.Context, joinCode string, playerID string) (*Party, error) {
	joinCode = strings.ToUpper(strings.TrimSpace(joinCode))
	playerID = strings.TrimSpace(playerID)
	if joinCode == "" || playerID == "" {
		return nil, fmt.Errorf("%w: join_code and player_id are required", ErrPartyRule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM players WHERE id = $1)`, playerID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: player %q does not exist", ErrPartyRule, playerID)
	}

	// Lock the party row to prevent concurrent joins exceeding MaxPartySize (6)
	var partyID, status string
	err = tx.QueryRowContext(ctx, `
		SELECT id, status FROM parties WHERE join_code = $1 FOR UPDATE
	`, joinCode).Scan(&partyID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPartyNotFound
	}
	if err != nil {
		return nil, err
	}

	if status != "FORMING" {
		return nil, fmt.Errorf("%w: party is no longer accepting members (status: %s)", ErrPartyRule, status)
	}

	// Check if already a member (idempotent join)
	var alreadyIn bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM party_members WHERE party_id = $1 AND player_id = $2)
	`, partyID, playerID).Scan(&alreadyIn); err != nil {
		return nil, err
	}
	if alreadyIn {
		_ = tx.Commit()
		return s.GetPartyByID(ctx, partyID)
	}

	var memberCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM party_members WHERE party_id = $1
	`, partyID).Scan(&memberCount); err != nil {
		return nil, err
	}
	if memberCount >= MaxPartySize {
		return nil, fmt.Errorf("%w: party is full (maximum %d members)", ErrPartyRule, MaxPartySize)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO party_members (party_id, player_id) VALUES ($1, $2)
	`, partyID, playerID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetPartyByID(ctx, partyID)
}

func (s *Service) GetPartyByID(ctx context.Context, partyID string) (*Party, error) {
	return s.queryParty(ctx, `SELECT id, join_code, leader_id, status, created_at FROM parties WHERE id = $1`, partyID)
}

func (s *Service) GetPartyByCode(ctx context.Context, joinCode string) (*Party, error) {
	return s.queryParty(ctx, `SELECT id, join_code, leader_id, status, created_at FROM parties WHERE join_code = $1`, strings.ToUpper(strings.TrimSpace(joinCode)))
}

func (s *Service) ListParties(ctx context.Context) ([]*Party, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM parties ORDER BY created_at DESC LIMIT 20`)
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

	var out []*Party
	for _, id := range ids {
		p, err := s.GetPartyByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Service) queryParty(ctx context.Context, query string, arg any) (*Party, error) {
	var p Party
	err := s.db.QueryRowContext(ctx, query, arg).Scan(&p.ID, &p.JoinCode, &p.LeaderID, &p.Status, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPartyNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT pm.player_id, pl.username, pl.character_class, pm.joined_at
		FROM party_members pm
		JOIN players pl ON pl.id = pm.player_id
		WHERE pm.party_id = $1
		ORDER BY pm.joined_at ASC
	`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	p.Members = []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.PlayerID, &m.Username, &m.CharacterClass, &m.JoinedAt); err != nil {
			return nil, err
		}
		p.Members = append(p.Members, m)
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
	mux.HandleFunc("GET /api/parties", h.handleListParties)
	mux.HandleFunc("POST /api/parties", h.handleCreateParty)
	mux.HandleFunc("POST /api/parties/join", h.handleJoinParty)
	mux.HandleFunc("GET /api/parties/{id}", h.handleGetParty)
}

func (h *Handler) handleListParties(w http.ResponseWriter, r *http.Request) {
	if code := r.URL.Query().Get("code"); code != "" {
		p, err := h.svc.GetPartyByCode(r.Context(), code)
		if errors.Is(err, ErrPartyNotFound) {
			httpx.Error(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.JSON(w, http.StatusOK, p)
		return
	}

	parties, err := h.svc.ListParties(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, parties)
}

func (h *Handler) handleCreateParty(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LeaderID string `json:"leader_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	p, err := h.svc.CreateParty(r.Context(), req.LeaderID)
	if errors.Is(err, ErrPartyRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusCreated, p)
}

func (h *Handler) handleJoinParty(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JoinCode string `json:"join_code"`
		PlayerID string `json:"player_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	p, err := h.svc.JoinPartyByCode(r.Context(), req.JoinCode, req.PlayerID)
	if errors.Is(err, ErrPartyNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrPartyRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h *Handler) handleGetParty(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetPartyByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrPartyNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}
