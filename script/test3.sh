# 1. Rebuild & restart backend
docker compose up --build -d

# 2. Fetch the settled run and inspect its first dropped reward
RUN_JSON=$(curl -s http://localhost:8080/api/runs | jq '.[0]')
RUN_ID=$(echo "$RUN_JSON" | jq -r '.id')
FIRST_ITEM_ID=$(echo "$RUN_JSON" | jq -r '.rewards[0].item_id')
FIRST_INSTANCE_ID=$(echo "$RUN_JSON" | jq -r '.rewards[0].instance_id')
HOLDER_ID=$(echo "$RUN_JSON" | jq -r '.rewards[0].current_owner_id')

# Pick a buyer from the run who is NOT the current holder
BUYER_ID="player-b"
if [ "$HOLDER_ID" = "player-b" ]; then BUYER_ID="player-c"; fi

echo "Run: $RUN_ID | Holder: $HOLDER_ID | Buyer: $BUYER_ID | Item: $FIRST_ITEM_ID ($FIRST_INSTANCE_ID)"

# 3. Buyer creates a Purchase Request for 750 Gold
REQ_JSON=$(curl -s -X POST "http://localhost:8080/api/runs/$RUN_ID/exchange-requests" \
  -H "Content-Type: application/json" \
  -d "{\"buyer_id\":\"$BUYER_ID\",\"target_item_id\":\"$FIRST_ITEM_ID\",\"offered_gold\":750}")
echo "$REQ_JSON" | jq .
REQ_ID=$(echo "$REQ_JSON" | jq -r '.id')

# 4. Holder accepts the Purchase Request
curl -s -X POST "http://localhost:8080/api/exchange-requests/$REQ_ID/accept" \
  -H "Content-Type: application/json" \
  -d "{\"seller_id\":\"$HOLDER_ID\",\"item_instance_id\":\"$FIRST_INSTANCE_ID\"}" | jq .

# 5. Inspect the Economy Audit Ledger for this Run
curl -s "http://localhost:8080/api/ledger?run_id=$RUN_ID" | jq .