package exchange

import (
	"context"
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
	ErrExchangeNotFound = errors.New("exchange request not found")
	ErrExchangeConflict = errors.New("exchange conflict or already fulfilled")
	ErrExchangeRule     = errors.New("exchange rule violation")
)

type ExchangeRequest struct {
	ID                      string     `json:"id"`
	RunID                   string     `json:"run_id"`
	BuyerID                 string     `json:"buyer_id"`
	BuyerUsername           string     `json:"buyer_username,omitempty"`
	TargetItemID            string     `json:"target_item_id"`
	TargetItemName          string     `json:"target_item_name,omitempty"`
	TargetItemRarity        string     `json:"target_item_rarity,omitempty"`
	OfferedGold             int64      `json:"offered_gold"`
	Status                  string     `json:"status"`
	FulfilledBySellerID     *string    `json:"fulfilled_by_seller_id,omitempty"`
	FulfilledItemInstanceID *string    `json:"fulfilled_item_instance_id,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`
}

type LedgerEntry struct {
	ID                string    `json:"id"`
	RunID             string    `json:"run_id"`
	EventType         string    `json:"event_type"`
	ExchangeRequestID *string   `json:"exchange_request_id,omitempty"`
	ItemInstanceID    *string   `json:"item_instance_id,omitempty"`
	ItemID            *string   `json:"item_id,omitempty"`
	ItemName          *string   `json:"item_name,omitempty"`
	FromPlayerID      *string   `json:"from_player_id,omitempty"`
	ToPlayerID        string    `json:"to_player_id"`
	GoldTransferred   int64     `json:"gold_transferred"`
	BuyerGoldBefore   *int64    `json:"buyer_gold_before,omitempty"`
	BuyerGoldAfter    *int64    `json:"buyer_gold_after,omitempty"`
	SellerGoldBefore  *int64    `json:"seller_gold_before,omitempty"`
	SellerGoldAfter   *int64    `json:"seller_gold_after,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type CreateRequestInput struct {
	RunID        string `json:"run_id"`
	BuyerID      string `json:"buyer_id"`
	TargetItemID string `json:"target_item_id"`
	OfferedGold  int64  `json:"offered_gold"`
}

type AcceptRequestInput struct {
	RequestID      string `json:"request_id"`
	SellerID       string `json:"seller_id"`
	ItemInstanceID string `json:"item_instance_id"`
}

type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// CreatePurchaseRequest creates a post-run purchase offer (max 1 per participant per run).
func (s *Service) CreatePurchaseRequest(ctx context.Context, input CreateRequestInput) (*ExchangeRequest, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	input.BuyerID = strings.TrimSpace(input.BuyerID)
	input.TargetItemID = strings.TrimSpace(input.TargetItemID)

	if input.RunID == "" || input.BuyerID == "" || input.TargetItemID == "" {
		return nil, fmt.Errorf("%w: run_id, buyer_id, and target_item_id are required", ErrExchangeRule)
	}
	if input.OfferedGold <= 0 {
		return nil, fmt.Errorf("%w: offered_gold must be > 0", ErrExchangeRule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. Lock DungeonRun & verify status and active Exchange Window
	var status string
	var expiresAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT status, exchange_expires_at
		FROM dungeon_runs
		WHERE id = $1
		FOR UPDATE
	`, input.RunID).Scan(&status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: dungeon run not found", ErrExchangeRule)
	}
	if err != nil {
		return nil, err
	}

	if status != "SETTLED_SUCCESS" {
		return nil, fmt.Errorf("%w: exchange is only available for SETTLED_SUCCESS runs (current: %s)", ErrExchangeRule, status)
	}

	now := time.Now().UTC()
	if !expiresAt.Valid || !now.Before(expiresAt.Time) {
		// Automatically mark any open requests on this run as EXPIRED
		_, _ = tx.ExecContext(ctx, `
			UPDATE exchange_requests SET status = 'EXPIRED'
			WHERE run_id = $1 AND status = 'OPEN'
		`, input.RunID)
		_ = tx.Commit()
		return nil, fmt.Errorf("%w: post-dungeon exchange window has expired", ErrExchangeRule)
	}

	// 2. Verify Buyer was an eligible participant of this Run
	var isParticipant bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM run_participants WHERE run_id = $1 AND player_id = $2)
	`, input.RunID, input.BuyerID).Scan(&isParticipant); err != nil {
		return nil, err
	}
	if !isParticipant {
		return nil, fmt.Errorf("%w: buyer %q was not a participant in this dungeon run", ErrExchangeRule, input.BuyerID)
	}

	// 3. Verify TargetItemID exists in catalog (and optionally dropped in this run)
	var itemExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM item_catalog WHERE id = $1)
	`, input.TargetItemID).Scan(&itemExists); err != nil {
		return nil, err
	}
	if !itemExists {
		return nil, fmt.Errorf("%w: target_item_id %q does not exist in catalog", ErrExchangeRule, input.TargetItemID)
	}

	// 4. Check if Buyer already has a request for this Run (Idempotent if identical, otherwise reject max 1 per run)
	var existingID, existingItem string
	var existingGold int64
	err = tx.QueryRowContext(ctx, `
		SELECT id, target_item_id, offered_gold
		FROM exchange_requests
		WHERE run_id = $1 AND buyer_id = $2
	`, input.RunID, input.BuyerID).Scan(&existingID, &existingItem, &existingGold)
	if err == nil {
		// If identical retry, return existing request idempotently
		if existingItem == input.TargetItemID && existingGold == input.OfferedGold {
			_ = tx.Rollback()
			return s.GetRequestByID(ctx, existingID)
		}
		return nil, fmt.Errorf("%w: participant %q already created a purchase request for this run (max 1 allowed)", ErrExchangeConflict, input.BuyerID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 5. Verify Buyer currently has enough Gold to make a credible offer
	var buyerGold int64
	if err := tx.QueryRowContext(ctx, `
		SELECT gold_balance FROM players WHERE id = $1
	`, input.BuyerID).Scan(&buyerGold); err != nil {
		return nil, err
	}
	if buyerGold < input.OfferedGold {
		return nil, fmt.Errorf("%w: buyer has insufficient gold (%d) to offer %d", ErrExchangeRule, buyerGold, input.OfferedGold)
	}

	reqID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO exchange_requests (id, run_id, buyer_id, target_item_id, offered_gold, status, created_at)
		VALUES ($1, $2, $3, $4, $5, 'OPEN', $6)
	`, reqID, input.RunID, input.BuyerID, input.TargetItemID, input.OfferedGold, now)
	if err != nil {
		return nil, fmt.Errorf("insert exchange_request: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetRequestByID(ctx, reqID)
}

// AcceptPurchaseRequest atomically swaps the eligible Item Instance and Gold between Buyer and Seller.
// Uses strict pessimistic locking in deterministic order to prevent race conditions, double-spends, or deadlocks.
func (s *Service) AcceptPurchaseRequest(ctx context.Context, input AcceptRequestInput) (*ExchangeRequest, error) {
	input.RequestID = strings.TrimSpace(input.RequestID)
	input.SellerID = strings.TrimSpace(input.SellerID)
	input.ItemInstanceID = strings.TrimSpace(input.ItemInstanceID)

	if input.RequestID == "" || input.SellerID == "" || input.ItemInstanceID == "" {
		return nil, fmt.Errorf("%w: request_id, seller_id, and item_instance_id are required", ErrExchangeRule)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. Lock the ExchangeRequest row first (`FOR UPDATE`) to serialize concurrent Accept attempts!
	var runID, buyerID, targetItemID, status string
	var offeredGold int64
	var fulfilledSeller, fulfilledInstance sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, buyer_id, target_item_id, offered_gold, status,
		       fulfilled_by_seller_id, fulfilled_item_instance_id
		FROM exchange_requests
		WHERE id = $1
		FOR UPDATE
	`, input.RequestID).Scan(
		&runID, &buyerID, &targetItemID, &offeredGold, &status,
		&fulfilledSeller, &fulfilledInstance,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExchangeNotFound
	}
	if err != nil {
		return nil, err
	}

	// IDEMPOTENCY CHECK: If this exact seller + item instance already fulfilled this request, return 200 OK!
	if status == "COMPLETED" &&
		fulfilledSeller.Valid && fulfilledSeller.String == input.SellerID &&
		fulfilledInstance.Valid && fulfilledInstance.String == input.ItemInstanceID {
		_ = tx.Rollback()
		return s.GetRequestByID(ctx, input.RequestID)
	}

	// RACE CONDITION GUARD: If another seller already fulfilled it (or it expired), reject with Conflict
	if status != "OPEN" {
		return nil, fmt.Errorf("%w: exchange request is no longer OPEN (status: %s)", ErrExchangeConflict, status)
	}

	if buyerID == input.SellerID {
		return nil, fmt.Errorf("%w: buyer cannot accept their own purchase request", ErrExchangeRule)
	}

	// 2. Check DungeonRun Exchange Window expiration
	var runStatus string
	var expiresAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT status, exchange_expires_at FROM dungeon_runs WHERE id = $1 FOR UPDATE
	`, runID).Scan(&runStatus, &expiresAt)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	if runStatus != "SETTLED_SUCCESS" || !expiresAt.Valid || !now.Before(expiresAt.Time) {
		_, _ = tx.ExecContext(ctx, `
			UPDATE exchange_requests SET status = 'EXPIRED'
			WHERE run_id = $1 AND status = 'OPEN'
		`, runID)
		_ = tx.Commit()
		return nil, fmt.Errorf("%w: post-dungeon exchange window has expired", ErrExchangeRule)
	}

	// 3. Verify Seller is an eligible Participant of the same Run
	var sellerIsParticipant bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM run_participants WHERE run_id = $1 AND player_id = $2)
	`, runID, input.SellerID).Scan(&sellerIsParticipant); err != nil {
		return nil, err
	}
	if !sellerIsParticipant {
		return nil, fmt.Errorf("%w: seller %q was not a participant in this dungeon run", ErrExchangeRule, input.SellerID)
	}

	// 4. Lock the Item Instance row (`FOR UPDATE`) and validate provenance & current ownership
	var instanceItemID, originRunID, currentOwnerID string
	err = tx.QueryRowContext(ctx, `
		SELECT item_id, origin_run_id, current_owner_id
		FROM item_instances
		WHERE id = $1
		FOR UPDATE
	`, input.ItemInstanceID).Scan(&instanceItemID, &originRunID, &currentOwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: item instance %q not found", ErrExchangeRule, input.ItemInstanceID)
	}
	if err != nil {
		return nil, err
	}

	if originRunID != runID {
		return nil, fmt.Errorf("%w: item instance was not obtained in this dungeon run", ErrExchangeRule)
	}
	if instanceItemID != targetItemID {
		return nil, fmt.Errorf("%w: item instance item_id (%s) does not match requested target_item_id (%s)", ErrExchangeRule, instanceItemID, targetItemID)
	}
	if currentOwnerID != input.SellerID {
		return nil, fmt.Errorf("%w: seller %q does not currently own item instance %q", ErrExchangeConflict, input.SellerID, input.ItemInstanceID)
	}

	// 5. Lock both Buyer and Seller player rows in deterministic ascending ID order to avoid deadlocks
	firstID, secondID := buyerID, input.SellerID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	pRows, err := tx.QueryContext(ctx, `
		SELECT id, gold_balance
		FROM players
		WHERE id IN ($1, $2)
		ORDER BY id ASC
		FOR UPDATE
	`, firstID, secondID)
	if err != nil {
		return nil, err
	}
	balances := make(map[string]int64, 2)
	for pRows.Next() {
		var pid string
		var bal int64
		if err := pRows.Scan(&pid, &bal); err != nil {
			pRows.Close()
			return nil, err
		}
		balances[pid] = bal
	}
	pRows.Close()

	buyerGoldBefore := balances[buyerID]
	sellerGoldBefore := balances[input.SellerID]

	// 6. Verify Buyer STILL has enough gold at the exact moment of acceptance!
	if buyerGoldBefore < offeredGold {
		return nil, fmt.Errorf(
			"%w: buyer %q currently has %d gold, which is less than offered_gold %d",
			ErrExchangeConflict, buyerID, buyerGoldBefore, offeredGold,
		)
	}

	buyerGoldAfter := buyerGoldBefore - offeredGold
	sellerGoldAfter := sellerGoldBefore + offeredGold

	// 7. Execute Atomic Economic Settlement (Deduct Buyer Gold, Credit Seller Gold, Transfer Item, Close Request)
	if _, err := tx.ExecContext(ctx, `UPDATE players SET gold_balance = $1 WHERE id = $2`, buyerGoldAfter, buyerID); err != nil {
		return nil, fmt.Errorf("deduct buyer gold: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE players SET gold_balance = $1 WHERE id = $2`, sellerGoldAfter, input.SellerID); err != nil {
		return nil, fmt.Errorf("credit seller gold: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE item_instances SET current_owner_id = $1 WHERE id = $2`, buyerID, input.ItemInstanceID); err != nil {
		return nil, fmt.Errorf("transfer item ownership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE exchange_requests
		SET status = 'COMPLETED',
		    fulfilled_by_seller_id = $1,
		    fulfilled_item_instance_id = $2,
		    completed_at = $3
		WHERE id = $4
	`, input.SellerID, input.ItemInstanceID, now, input.RequestID); err != nil {
		return nil, fmt.Errorf("complete exchange request: %w", err)
	}

	// 8. Write Immutable Economy Audit Ledger Entry
	ledgerID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO economy_ledger (
			id, run_id, event_type, exchange_request_id, item_instance_id,
			from_player_id, to_player_id, gold_transferred,
			buyer_gold_before, buyer_gold_after, seller_gold_before, seller_gold_after, created_at
		) VALUES ($1, $2, 'EXCHANGE_SETTLEMENT', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		ledgerID, runID, input.RequestID, input.ItemInstanceID,
		input.SellerID, buyerID, offeredGold,
		buyerGoldBefore, buyerGoldAfter, sellerGoldBefore, sellerGoldAfter, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert economy_ledger: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetRequestByID(ctx, input.RequestID)
}

// AdjustPlayerGold is a helper used by tests and the operator simulator to test
// the scenario where a player's Gold balance changes between creating a request and its acceptance.
func (s *Service) AdjustPlayerGold(ctx context.Context, playerID string, newBalance int64) error {
	if newBalance < 0 {
		return fmt.Errorf("%w: gold_balance cannot be set below 0", ErrExchangeRule)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE players SET gold_balance = $1 WHERE id = $2`, newBalance, playerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: player not found", ErrExchangeRule)
	}
	return nil
}

func (s *Service) expireStaleRequestsForRun(ctx context.Context, runID string) {
	_, _ = s.db.ExecContext(ctx, `
		UPDATE exchange_requests er
		SET status = 'EXPIRED'
		FROM dungeon_runs dr
		WHERE er.run_id = dr.id
		  AND er.run_id = $1
		  AND er.status = 'OPEN'
		  AND dr.exchange_expires_at IS NOT NULL
		  AND dr.exchange_expires_at <= NOW()
	`, runID)
}

func (s *Service) GetRequestByID(ctx context.Context, requestID string) (*ExchangeRequest, error) {
	var req ExchangeRequest
	err := s.db.QueryRowContext(ctx, `
		SELECT er.id, er.run_id, er.buyer_id, p.username,
		       er.target_item_id, ic.name, ic.rarity, er.offered_gold, er.status,
		       er.fulfilled_by_seller_id, er.fulfilled_item_instance_id,
		       er.created_at, er.completed_at
		FROM exchange_requests er
		JOIN players p ON p.id = er.buyer_id
		JOIN item_catalog ic ON ic.id = er.target_item_id
		WHERE er.id = $1
	`, requestID).Scan(
		&req.ID, &req.RunID, &req.BuyerID, &req.BuyerUsername,
		&req.TargetItemID, &req.TargetItemName, &req.TargetItemRarity, &req.OfferedGold, &req.Status,
		&req.FulfilledBySellerID, &req.FulfilledItemInstanceID,
		&req.CreatedAt, &req.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExchangeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Service) ListRequestsByRun(ctx context.Context, runID string) ([]ExchangeRequest, error) {
	s.expireStaleRequestsForRun(ctx, runID)

	rows, err := s.db.QueryContext(ctx, `
		SELECT er.id, er.run_id, er.buyer_id, p.username,
		       er.target_item_id, ic.name, ic.rarity, er.offered_gold, er.status,
		       er.fulfilled_by_seller_id, er.fulfilled_item_instance_id,
		       er.created_at, er.completed_at
		FROM exchange_requests er
		JOIN players p ON p.id = er.buyer_id
		JOIN item_catalog ic ON ic.id = er.target_item_id
		WHERE er.run_id = $1
		ORDER BY er.created_at DESC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExchangeRequest{}
	for rows.Next() {
		var req ExchangeRequest
		if err := rows.Scan(
			&req.ID, &req.RunID, &req.BuyerID, &req.BuyerUsername,
			&req.TargetItemID, &req.TargetItemName, &req.TargetItemRarity, &req.OfferedGold, &req.Status,
			&req.FulfilledBySellerID, &req.FulfilledItemInstanceID,
			&req.CreatedAt, &req.CompletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func (s *Service) ListLedgerByRun(ctx context.Context, runID string) ([]LedgerEntry, error) {
	query := `
		SELECT el.id, el.run_id, el.event_type, el.exchange_request_id,
		       el.item_instance_id, ii.item_id, ic.name,
		       el.from_player_id, el.to_player_id, el.gold_transferred,
		       el.buyer_gold_before, el.buyer_gold_after,
		       el.seller_gold_before, el.seller_gold_after, el.created_at
		FROM economy_ledger el
		LEFT JOIN item_instances ii ON ii.id = el.item_instance_id
		LEFT JOIN item_catalog ic ON ic.id = ii.item_id
	`
	var rows *sql.Rows
	var err error
	if runID != "" {
		rows, err = s.db.QueryContext(ctx, query+` WHERE el.run_id = $1 ORDER BY el.created_at ASC`, runID)
	} else {
		rows, err = s.db.QueryContext(ctx, query+` ORDER BY el.created_at DESC LIMIT 50`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(
			&e.ID, &e.RunID, &e.EventType, &e.ExchangeRequestID,
			&e.ItemInstanceID, &e.ItemID, &e.ItemName,
			&e.FromPlayerID, &e.ToPlayerID, &e.GoldTransferred,
			&e.BuyerGoldBefore, &e.BuyerGoldAfter,
			&e.SellerGoldBefore, &e.SellerGoldAfter, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs/{id}/exchange-requests", h.handleListByRun)
	mux.HandleFunc("POST /api/runs/{id}/exchange-requests", h.handleCreateRequest)
	mux.HandleFunc("POST /api/exchange-requests/{id}/accept", h.handleAcceptRequest)
	mux.HandleFunc("GET /api/ledger", h.handleListLedger)
	mux.HandleFunc("POST /api/players/{id}/gold", h.handleAdjustGold)
}

func (h *Handler) handleListByRun(w http.ResponseWriter, r *http.Request) {
	reqs, err := h.svc.ListRequestsByRun(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, reqs)
}

func (h *Handler) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	var input CreateRequestInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	input.RunID = r.PathValue("id")

	req, err := h.svc.CreatePurchaseRequest(r.Context(), input)
	if errors.Is(err, ErrExchangeConflict) {
		httpx.Error(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrExchangeRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusCreated, req)
}

func (h *Handler) handleAcceptRequest(w http.ResponseWriter, r *http.Request) {
	var input AcceptRequestInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	input.RequestID = r.PathValue("id")

	req, err := h.svc.AcceptPurchaseRequest(r.Context(), input)
	if errors.Is(err, ErrExchangeNotFound) {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrExchangeConflict) {
		httpx.Error(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrExchangeRule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, req)
}

func (h *Handler) handleListLedger(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	entries, err := h.svc.ListLedgerByRun(r.Context(), runID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, entries)
}

func (h *Handler) handleAdjustGold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GoldBalance int64 `json:"gold_balance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := h.svc.AdjustPlayerGold(r.Context(), r.PathValue("id"), body.GoldBalance); err != nil {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"player_id": r.PathValue("id"), "gold_balance": body.GoldBalance})
}