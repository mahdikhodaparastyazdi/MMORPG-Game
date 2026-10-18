package app

import (
	"database/sql"
	"net/http"

	"dungeon-loot-backend/internal/config"
	"dungeon-loot-backend/internal/modules/dungeon"
	"dungeon-loot-backend/internal/modules/exchange"
	"dungeon-loot-backend/internal/modules/lootconfig"
	"dungeon-loot-backend/internal/modules/party"
	"dungeon-loot-backend/internal/modules/player"
	"dungeon-loot-backend/internal/modules/settlement"
	"dungeon-loot-backend/internal/shared/httpx"
)

type Application struct {
	Config        config.Config
	DB            *sql.DB
	Mux           *http.ServeMux
	LootConfigSvc *lootconfig.Service
	PlayerSvc     *player.Service
	PartySvc      *party.Service
	DungeonSvc    *dungeon.Service
	SettlementSvc *settlement.Service
	ExchangeSvc   *exchange.Service
}

func New(cfg config.Config, database *sql.DB) *Application {
	mux := http.NewServeMux()

	// 1. Health & Readiness Endpoints
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := database.PingContext(r.Context()); err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "db_unreachable")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// 2. Instantiate Domain Services (Dependency Injection)
	lootConfigSvc := lootconfig.NewService(database)
	playerSvc := player.NewService(database)
	partySvc := party.NewService(database)
	dungeonSvc := dungeon.NewService(database, lootConfigSvc)
	settlementSvc := settlement.NewService(
		database,
		lootConfigSvc,
		dungeonSvc,
		cfg.ExchangeWindow,
		cfg.GameplayServerSecret,
	)
	exchangeSvc := exchange.NewService(database)

	// 3. Register Module HTTP Handlers
	lootconfig.NewHandler(lootConfigSvc).RegisterRoutes(mux)
	player.NewHandler(playerSvc).RegisterRoutes(mux)
	party.NewHandler(partySvc).RegisterRoutes(mux)
	dungeon.NewHandler(dungeonSvc).RegisterRoutes(mux)
	settlement.NewHandler(settlementSvc, cfg.GameplayServerSecret).RegisterRoutes(mux)
	exchange.NewHandler(exchangeSvc).RegisterRoutes(mux)

	// 4. Serve Minimal Testing UI at root "/"
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return &Application{
		Config:        cfg,
		DB:            database,
		Mux:           mux,
		LootConfigSvc: lootConfigSvc,
		PlayerSvc:     playerSvc,
		PartySvc:      partySvc,
		DungeonSvc:    dungeonSvc,
		SettlementSvc: settlementSvc,
		ExchangeSvc:   exchangeSvc,
	}
}
