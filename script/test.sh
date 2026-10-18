# 1. Run tests and rebuild container
go test -v ./internal/modules/lootconfig/...
docker compose up --build -d

# 2. Player A creates a Party
PARTY_JSON=$(curl -s -X POST http://localhost:8080/api/parties \
  -H "Content-Type: application/json" \
  -d '{"leader_id": "player-a"}')
echo "$PARTY_JSON" | jq .

PARTY_ID=$(echo "$PARTY_JSON" | jq -r .id)
JOIN_CODE=$(echo "$PARTY_JSON" | jq -r .join_code)

# 3. Try starting with only 1 member (Must fail with < 3 members error!)
curl -s -X POST http://localhost:8080/api/runs/start \
  -H "Content-Type: application/json" \
  -d "{\"party_id\":\"$PARTY_ID\",\"leader_id\":\"player-a\",\"dungeon_id\":\"dungeon-x\",\"difficulty\":18}" | jq .

# 4. Players B, C, and D join using the short Join Code
for P in player-b player-c player-d; do
  curl -s -X POST http://localhost:8080/api/parties/join \
    -H "Content-Type: application/json" \
    -d "{\"join_code\":\"$JOIN_CODE\",\"player_id\":\"$P\"}" > /dev/null
done

# 5. Leader (Player A) starts Dungeon X at Difficulty 18 (Succeeds!)
curl -s -X POST http://localhost:8080/api/runs/start \
  -H "Content-Type: application/json" \
  -d "{\"party_id\":\"$PARTY_ID\",\"leader_id\":\"player-a\",\"dungeon_id\":\"dungeon-x\",\"difficulty\":18}" | jq .