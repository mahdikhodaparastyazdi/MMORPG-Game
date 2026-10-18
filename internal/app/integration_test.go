package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dungeon-loot-backend/internal/config"
	"dungeon-loot-backend/internal/db"
	"dungeon-loot-backend/internal/modules/dungeon"
	"dungeon-loot-backend/internal/modules/exchange"
	"dungeon-loot-backend/internal/modules/lootconfig"
	"dungeon-loot-backend/internal/modules/settlement"
)

func setupTestApp(t *testing.T) (*Application, func()) {
	t.Helper()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = "postgres://lootuser:lootpass@localhost:5432/lootdb?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	migrationPath := filepath.Join("..", "..", "migrations", "001_init.sql")
	database, err := db.ConnectAndMigrate(ctx, dbURL, migrationPath)
	if err != nil {
		t.Skipf("skipping integration test (database not reachable at %s): %v", dbURL, err)
	}

	resetDatabaseState(t, database, migrationPath)

	cfg := config.Config{
		Port:                 "0",
		DatabaseURL:          dbURL,
		GameplayServerSecret: "test-secret",
		ExchangeWindow:       5 * time.Minute,
	}

	application := New(cfg, database)
	cleanup := func() {
		_ = database.Close()
	}
	return application, cleanup
}

func resetDatabaseState(t *testing.T, database *sql.DB, migrationPath string) {
	t.Helper()
	ctx := context.Background()
	_, err := database.ExecContext(ctx, `
		TRUNCATE TABLE
			economy_ledger,
			settlement_audit_logs,
			exchange_requests,
			item_instances,
			run_participants,
			dungeon_runs,
			party_members,
			parties,
			loot_config_items,
			loot_config_rarities,
			loot_configs,
			item_catalog,
			players
		RESTART IDENTITY CASCADE;
	`)
	if err != nil {
		t.Fatalf("truncate tables: %v", err)
	}

	sqlBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("re-seed database: %v", err)
	}
}

// Helper to create a 4-player party (player-a, player-b, player-c, player-d) and start Dungeon X (Lv 18)
func createFourPlayerRun(t *testing.T, a *Application) *dungeon.DungeonRun {
	t.Helper()
	ctx := context.Background()

	pt, err := a.PartySvc.CreateParty(ctx, "player-a")
	if err != nil {
		t.Fatalf("CreateParty: %v", err)
	}
	for _, pid := range []string{"player-b", "player-c", "player-d"} {
		if _, err := a.PartySvc.JoinPartyByCode(ctx, pt.JoinCode, pid); err != nil {
			t.Fatalf("JoinPartyByCode(%s): %v", pid, err)
		}
	}

	run, err := a.DungeonSvc.StartRun(ctx, dungeon.StartRunInput{
		PartyID:    pt.ID,
		LeaderID:   "player-a",
		DungeonID:  "dungeon-x",
		Difficulty: 18,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return run
}

// 1. Test Idempotency & Concurrent Retries on Dungeon Completion
func TestIdempotentConcurrentDungeonSettlement(t *testing.T) {
	a, cleanup := setupTestApp(t)
	defer cleanup()

	run := createFourPlayerRun(t, a)
	seed := int64(99999)

	input := settlement.CompletionInput{
		RunID:      run.ID,
		DungeonID:  "dungeon-x",
		Difficulty: 18,
		Success:    true,
		Contributions: map[string]float64{
			"player-a": 1200,
			"player-b": 800,
			"player-c": 400,
			"player-d": 100,
		},
		OptionalSeed: &seed,
	}

	// Fire 10 concurrent settlement requests for the exact same run_id
	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	startGate := make(chan struct{})
	results := make([]*dungeon.DungeonRun, workers)
	errs := make([]error, workers)

	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startGate
			results[idx], errs[idx] = a.SettlementSvc.SettleRun(context.Background(), input)
		}(i)
	}
	close(startGate)
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d failed idempotent SettleRun: %v", i, errs[i])
		}
		if len(results[i].Rewards) == 0 {
			t.Fatalf("worker %d returned 0 rewards", i)
		}
		if len(results[i].Rewards) != len(results[0].Rewards) {
			t.Fatalf("worker %d reward count mismatch: %d vs %d", i, len(results[i].Rewards), len(results[0].Rewards))
		}
	}

	// Verify database has the exact expected number of item_instances (no duplicates!)
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM item_instances WHERE origin_run_id = $1`, run.ID).Scan(&count); err != nil {
		t.Fatalf("query item_instances count: %v", err)
	}
	if count != len(results[0].Rewards) {
		t.Fatalf("expected exactly %d persisted item instances, found %d", len(results[0].Rewards), count)
	}
}

// 2. Test Concurrent Accept Race Condition (Two eligible holders click Accept at the exact same time)
func TestConcurrentExchangeAcceptRaceCondition(t *testing.T) {
	a, cleanup := setupTestApp(t)
	defer cleanup()
	ctx := context.Background()

	// Configure dungeon-x Lv 18 so ONLY "item-shadow-blade" can drop (100% EPIC, 1 item)
	// so multiple participants are guaranteed to receive the same Item ID in this run.
	_, err := a.LootConfigSvc.UpsertActiveConfig(ctx, lootconfig.UpsertConfigInput{
		DungeonID:          "dungeon-x",
		Difficulty:         18,
		AllocationMode:     lootconfig.AllocationPersonalLoot,
		ContributionMode:   lootconfig.ContributionIndependent,
		PartyPoolDropCount: 4,
		RarityChances: map[lootconfig.Rarity]float64{
			lootconfig.RarityEpic: 1.0,
		},
		EligibleItems: []lootconfig.EligibleItem{
			{ItemID: "item-shadow-blade", Weight: 100},
		},
	})
	if err != nil {
		t.Fatalf("UpsertActiveConfig: %v", err)
	}

	run := createFourPlayerRun(t, a)
	settled, err := a.SettlementSvc.SettleRun(ctx, settlement.CompletionInput{
		RunID:      run.ID,
		DungeonID:  "dungeon-x",
		Difficulty: 18,
		Success:    true,
		Contributions: map[string]float64{
			"player-a": 100, "player-b": 100, "player-c": 100, "player-d": 100,
		},
	})
	if err != nil {
		t.Fatalf("SettleRun: %v", err)
	}

	// Find two distinct sellers (player-c and player-d) who both received item-shadow-blade
	var sellerCInstance, sellerDInstance string
	for _, rw := range settled.Rewards {
		if rw.CurrentOwnerID == "player-c" {
			sellerCInstance = rw.InstanceID
		}
		if rw.CurrentOwnerID == "player-d" {
			sellerDInstance = rw.InstanceID
		}
	}
	if sellerCInstance == "" || sellerDInstance == "" {
		t.Fatalf("expected both player-c and player-d to hold an instance of item-shadow-blade")
	}

	// Player B (starts with 1000 Gold) creates a Purchase Request for item-shadow-blade at 750 Gold
	req, err := a.ExchangeSvc.CreatePurchaseRequest(ctx, exchange.CreateRequestInput{
		RunID:        run.ID,
		BuyerID:      "player-b",
		TargetItemID: "item-shadow-blade",
		OfferedGold:  750,
	})
	if err != nil {
		t.Fatalf("CreatePurchaseRequest: %v", err)
	}

	// Players C and D attempt to Accept simultaneously!
	var wg sync.WaitGroup
	wg.Add(2)
	startGate := make(chan struct{})
	var successCount, conflictCount int32

	acceptWorker := func(sellerID, instanceID string) {
		defer wg.Done()
		<-startGate
		_, err := a.ExchangeSvc.AcceptPurchaseRequest(context.Background(), exchange.AcceptRequestInput{
			RequestID:      req.ID,
			SellerID:       sellerID,
			ItemInstanceID: instanceID,
		})
		if err == nil {
			atomic.AddInt32(&successCount, 1)
		} else if errors.Is(err, exchange.ErrExchangeConflict) {
			atomic.AddInt32(&conflictCount, 1)
		} else {
			t.Errorf("unexpected error from seller %s: %v", sellerID, err)
		}
	}

	go acceptWorker("player-c", sellerCInstance)
	go acceptWorker("player-d", sellerDInstance)
	close(startGate)
	wg.Wait()

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("expected exactly 1 success and 1 conflict, got success=%d conflict=%d", successCount, conflictCount)
	}

	// Verify Player B's gold was deducted only ONCE (1000 - 750 = 250 Gold)
	buyer, err := a.PlayerSvc.GetPlayerWithInventory(ctx, "player-b")
	if err != nil {
		t.Fatalf("GetPlayerWithInventory: %v", err)
	}
	if buyer.GoldBalance != 250 {
		t.Fatalf("expected buyer gold balance to be 250, got %d", buyer.GoldBalance)
	}
}

// 3. Test Buyer Gold Balance Drops Between Request Creation and Acceptance
func TestExchangeFailsSafelyWhenBuyerGoldDropsBeforeAccept(t *testing.T) {
	a, cleanup := setupTestApp(t)
	defer cleanup()
	ctx := context.Background()

	run := createFourPlayerRun(t, a)
	settled, err := a.SettlementSvc.SettleRun(ctx, settlement.CompletionInput{
		RunID:      run.ID,
		DungeonID:  "dungeon-x",
		Difficulty: 18,
		Success:    true,
		Contributions: map[string]float64{
			"player-a": 500, "player-b": 500, "player-c": 500, "player-d": 500,
		},
	})
	if err != nil {
		t.Fatalf("SettleRun: %v", err)
	}

	targetReward := settled.Rewards[0]
	sellerID := targetReward.CurrentOwnerID
	buyerID := "player-b"
	if sellerID == "player-b" {
		buyerID = "player-a"
	}

	// Buyer creates a valid request for 750 Gold
	req, err := a.ExchangeSvc.CreatePurchaseRequest(ctx, exchange.CreateRequestInput{
		RunID:        run.ID,
		BuyerID:      buyerID,
		TargetItemID: targetReward.ItemID,
		OfferedGold:  750,
	})
	if err != nil {
		t.Fatalf("CreatePurchaseRequest: %v", err)
	}

	// Before Seller accepts, Buyer's Gold balance drops to 400 Gold!
	if err := a.ExchangeSvc.AdjustPlayerGold(ctx, buyerID, 400); err != nil {
		t.Fatalf("AdjustPlayerGold: %v", err)
	}

	// Seller attempts to accept -> MUST fail with ErrExchangeConflict and leave balances & item ownership untouched
	_, err = a.ExchangeSvc.AcceptPurchaseRequest(ctx, exchange.AcceptRequestInput{
		RequestID:      req.ID,
		SellerID:       sellerID,
		ItemInstanceID: targetReward.InstanceID,
	})
	if !errors.Is(err, exchange.ErrExchangeConflict) {
		t.Fatalf("expected ErrExchangeConflict when buyer gold is insufficient at accept time, got: %v", err)
	}

	buyerAfter, _ := a.PlayerSvc.GetPlayerWithInventory(ctx, buyerID)
	if buyerAfter.GoldBalance != 400 {
		t.Fatalf("expected buyer gold to remain 400, got %d", buyerAfter.GoldBalance)
	}
}

// 4. Test Configuration Immutability for In-Progress and Settled Runs
func TestConfigUpdateDoesNotAffectActiveOrSettledRuns(t *testing.T) {
	a, cleanup := setupTestApp(t)
	defer cleanup()
	ctx := context.Background()

	// Start Run while v1 config is active
	run := createFourPlayerRun(t, a)
	pinnedConfigID := run.LootConfigID

	// Now update the active config for dungeon-x Lv 18 to v2 (different mode & items) while the run is ACTIVE
	newCfg, err := a.LootConfigSvc.UpsertActiveConfig(ctx, lootconfig.UpsertConfigInput{
		DungeonID:          "dungeon-x",
		Difficulty:         18,
		AllocationMode:     lootconfig.AllocationPersonalLoot,
		ContributionMode:   lootconfig.ContributionIndependent,
		PartyPoolDropCount: 1,
		RarityChances:      map[lootconfig.Rarity]float64{lootconfig.RarityCommon: 1.0},
		EligibleItems:      []lootconfig.EligibleItem{{ItemID: "item-iron-sword", Weight: 100}},
	})
	if err != nil {
		t.Fatalf("UpsertActiveConfig v2: %v", err)
	}
	if newCfg.ID == pinnedConfigID {
		t.Fatalf("expected v2 config to have a new immutable ID")
	}

	// Settle the in-progress run -> must still use pinnedConfigID (v1)
	settled, err := a.SettlementSvc.SettleRun(ctx, settlement.CompletionInput{
		RunID:      run.ID,
		DungeonID:  "dungeon-x",
		Difficulty: 18,
		Success:    true,
		Contributions: map[string]float64{
			"player-a": 1000, "player-b": 500, "player-c": 250, "player-d": 100,
		},
	})
	if err != nil {
		t.Fatalf("SettleRun: %v", err)
	}

	if settled.LootConfigID != pinnedConfigID {
		t.Fatalf("expected settled run to retain pinned config %s, got %s", pinnedConfigID, settled.LootConfigID)
	}
}
