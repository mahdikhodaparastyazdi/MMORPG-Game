package lootconfig

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/catalog", h.handleListCatalog)
	mux.HandleFunc("GET /api/loot-configs", h.handleGetConfigs)
	mux.HandleFunc("POST /api/loot-configs", h.handleUpsertConfig)
}

func (h *Handler) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListCatalog(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) handleGetConfigs(w http.ResponseWriter, r *http.Request) {
	dungeonID := r.URL.Query().Get("dungeon_id")
	diffStr := r.URL.Query().Get("difficulty")

	if dungeonID != "" && diffStr != "" {
		diff, err := strconv.Atoi(diffStr)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "difficulty must be an integer")
			return
		}
		cfg, err := h.svc.GetActiveConfig(r.Context(), dungeonID, diff)
		if errors.Is(err, ErrConfigNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, cfg)
		return
	}

	configs, err := h.svc.ListActiveConfigs(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, configs)
}

func (h *Handler) handleUpsertConfig(w http.ResponseWriter, r *http.Request) {
	var input UpsertConfigInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
		return
	}

	cfg, err := h.svc.UpsertActiveConfig(r.Context(), input)
	if errors.Is(err, ErrInvalidConfig) {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, cfg)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}