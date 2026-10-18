```markdown
# Final Implementation Notes, Assumptions & Post-Implementation Reflections

## 1. Explicit Assumptions Made for Open Requirements

Where the specification intentionally left behavioral details open to engineering judgment, we documented and implemented the following assumptions:

1. **Contribution-Aware Math & Guaranteed Minimum Floor (`ContributionFloor = 0.20`):**
   * *Ambiguity:* In `CONTRIBUTION_AWARE` mode, higher contribution must meaningfully increase reward chance while lower contributors still retain a non-zero chance[cite: 2].
   * *Decision:* We implemented a convex combination of an equal share ($1/N$) and the participant's normalized contribution share:
     $$\text{EffectiveWeight}_p = 0.20 \cdot \left(\frac{1}{N}\right) + 0.80 \cdot \left(\frac{C_p}{\sum C_k}\right)$$
     Even if a participant records `0` combat contribution (e.g., joined right before boss kill or disconnected briefly), they retain $20\%$ of an equal share in `PARTY_POOL` mode (and a $0.50\times$ floor multiplier in `PERSONAL_LOOT` mode), preventing toxic zero-loot edge cases while heavily rewarding top contributors[cite: 2].
2. **Pinning Loot Configuration at Run Start:**
   * *Decision:* `dungeon_runs` pins the active `loot_config_id` at the moment the Leader starts the dungeon (rather than looking up the latest config at completion time)[cite: 1, 2]. This guarantees players always receive rewards according to the exact ruleset active when they entered the instance and ensures mid-run operator edits never mutate active runs[cite: 2].
3. **One Purchase Request per Participant per Run Scope:**
   * *Ambiguity:* Whether a participant can create a second Purchase Request if their first one is cancelled or expires[cite: 2].
   * *Decision:* The specification states *"هر Participant در هر Run حداکثر میتواند یک Purchase Request داشته باشد"*[cite: 2]. We enforced a hard database constraint `UNIQUE (run_id, buyer_id)` on `exchange_requests`, allowing idempotent retries of the exact same request payload while rejecting second requests within the same run[cite: 2].
4. **Gold Balance Validation at Both Request Creation and Acceptance:**
   * *Decision:* We verify `buyer.gold_balance >= offered_gold` when creating the Purchase Request (to prevent spam offers) **and** re-verify it under a `SELECT ... FOR UPDATE` row lock at the moment a Seller accepts the request, ensuring concurrent gold spending never results in a negative balance[cite: 2, 4].

---

## 2. Architectural Changes & Reflections After Implementation

Comparing our initial design in `docs/architecture.md` with the completed implementation:

1. **Modular Monolith Dependency Boundaries:**
   * During implementation, structuring the Go codebase into isolated domain modules (`lootconfig`, `player`, `party`, `dungeon`, `settlement`, `exchange`) wired via `internal/app` proved superior to splitting into separate network processes for the vertical slice. It preserved strict domain boundaries while allowing atomic cross-domain SQL transactions (such as updating `item_instances`, `players.gold_balance`, `exchange_requests`, and `economy_ledger` in a single ACID commit)[cite: 2].
2. **Deterministic Deadlock Prevention in Exchange Settlement:**
   * In our initial architecture diagram, we noted locking the Buyer and Seller wallet rows during Exchange acceptance. During implementation, we refined this to explicitly sort the two player IDs (`ORDER BY id ASC FOR UPDATE`) before acquiring the PostgreSQL row locks. Without deterministic lock ordering, two players accepting each other's requests across different runs simultaneously could trigger a PostgreSQL deadlock.
3. **Lazy vs. Background Expiration of Exchange Requests:**
   * In the prototype, `exchange_expires_at` is enforced strictly on every write (`CreatePurchaseRequest` / `AcceptPurchaseRequest`) and lazily updated to `EXPIRED` on read queries (`ListRequestsByRun`)[cite: 2]. In a high-scale production environment where downstream analytics listen for expiration events, we would pair this hard timestamp check with a lightweight background ticker or pg_cron job.

---

## 3. What We Would Do Next With More Time (Production Roadmap)

1. **Transactional Outbox Pattern for NATS/Kafka Events:**
   * Currently, `economy_ledger` and `settlement_audit_logs` are persisted synchronously inside the PostgreSQL transaction[cite: 2]. To notify connected Game Clients in real time (via WebSockets) when loot is settled or a trade completes, we would add an `outbox_events` table polled by a relay worker publishing to NATS JetStream.
2. **Escrow Option vs. Non-Escrow Purchase Requests:**
   * Right now, creating a Purchase Request does not lock/escrow the buyer's gold (allowing the "buyer gold changes before acceptance" scenario described in the prompt). In production, reserving `reserved_gold` upon request creation (and releasing it on expiration) provides a better player UX so sellers never encounter failed acceptances due to a buyer spending their gold elsewhere.
3. **Automated Partition Management & Prometheus Metrics:**
   * Add `pg_partman` migrations for time-partitioning `economy_ledger` and `settlement_audit_logs`, and expose `/metrics` with Prometheus histograms for settlement lock wait times and idempotency hit rates[cite: 4].