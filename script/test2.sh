# 1. Run all unit tests
go test -v ./internal/modules/...

# 2. Rebuild and restart container
docker compose up --build -d

# 3. Get the most recent active Run ID
RUN_ID=$(curl -s http://localhost:8080/api/runs | jq -r '.[0].id')
echo "Testing Settlement on Run: $RUN_ID"

# 4. Try calling the internal Gameplay Server endpoint WITHOUT X-Server-Token (Must return 401 Unauthorized)
curl -i -X POST "http://localhost:8080/api/internal/gameplay/runs/$RUN_ID/complete" \
  -H "Content-Type: application/json" \
  -d '{"success": true}'

# 5. Submit Dungeon Completion WITH valid X-Server-Token
curl -s -X POST "http://localhost:8080/api/internal/gameplay/runs/$RUN_ID/complete" \
  -H "Content-Type: application/json" \
  -H "X-Server-Token: trusted-gameplay-server-secret-2026" \
  -d '{
    "dungeon_id": "dungeon-x",
    "difficulty": 18,
    "success": true,
    "contributions": {
      "player-a": 1200,
      "player-b": 850,
      "player-c": 400,
      "player-d": 150
    }
  }' | jq .

# 6. Retry the exact same Completion call a 2nd time (Idempotency check: returns identical rewards, 0 duplicates!)
curl -s -X POST "http://localhost:8080/api/internal/gameplay/runs/$RUN_ID/complete" \
  -H "Content-Type: application/json" \
  -H "X-Server-Token: trusted-gameplay-server-secret-2026" \
  -d '{
    "dungeon_id": "dungeon-x",
    "difficulty": 18,
    "success": true,
    "contributions": {
      "player-a": 1200,
      "player-b": 850,
      "player-c": 400,
      "player-d": 150
    }
  }' | jq '.rewards | length'