# AI Usage & Engineering Decisions Log

## 1. AI Tools Used & Role in Workflow
* **Tool:** Google Gemini
* **Workflow Integration:**
  * **Task Breakdown & Planning:** Decomposed the Persian/English specification into a 9-step sequential engineering plan respecting the Git commit order requirement (`docs/architecture.md` committed prior to code implementation)[cite: 1, 3].
  * **Architecture Drafting & Mermaid Diagrams:** Assisted in structuring the 50k CCU / 1M MAU system architecture, sequence diagrams, and trade-off matrices in `docs/architecture.md`[cite: 1, 3].
  * **Boilerplate & SQL/HTTP Scaffolding:** Generated initial SQL schema definitions, HTTP handler boilerplate, and the single-page HTML/JS testing console (`web/index.html`)[cite: 3].
  * **Test Case Generation:** Helped scaffold concurrent goroutine barrier tests (`sync.WaitGroup` + channel gates) for idempotent settlement and simultaneous exchange acceptance.

---

## 2. Key AI Suggestions Rejected, Modified, or Refined

Per the assignment guidelines, below are the significant engineering adjustments made over standard/initial AI patterns[cite: 3]:

1. **Rejected Flat Package Layout in Favor of a Modular Monolith (`internal/modules/*`):**
   * *Initial Approach:* Putting handlers and queries into a flat `internal/lootconfig` or single service package.
   * *Refinement:* Refactored the application into a strict **Modular Monolith** (`lootconfig`, `player`, `party`, `dungeon`, `settlement`, `exchange`) orchestrated by a central Dependency Injection container (`internal/app/app.go`). This mirrors the production service boundaries in `docs/architecture.md` while keeping local execution simple[cite: 1, 3].
2. **Rejected ORM (GORM) Magic for Critical Financial/Inventory Locks in Favor of Explicit `database/sql` + `pgx`:**
   * *Reasoning:* While ORMs are convenient for CRUD, subtle concurrency bugs often hide in ORM-generated queries. We used explicit parameterized SQL with `SELECT ... FOR UPDATE` and deterministic lock ordering (`ORDER BY id ASC FOR UPDATE` on player wallets) so every transactional lock boundary is transparent and verifiable during code review[cite: 2].
3. **Refined Loot Configuration Updates from In-Place `UPDATE` to Immutable Versioned Snapshots:**
   * *Initial Thought:* Updating weights or probabilities directly on a `dungeon_configs` row.
   * *Refinement:* Direct updates violate the requirement that changing loot configurations must not affect in-progress runs or corrupt historical auditability[cite: 2]. We redesigned `loot_configs` as append-only versioned records (`version = max + 1`) and pinned `loot_config_id` on `dungeon_runs` at run start[cite: 2].
4. **Decoupled Two-Stage Loot Roll Verification:**
   * *Refinement:* Ensured Stage 1 (Rarity roll against `loot_config_rarities`) and Stage 2 (Intra-rarity weighted item roll against `loot_config_items`) are mathematically isolated, and wrote a deterministic seed test (`TestExecuteLootRolls_RarityProbabilityRemainsConstantWhenItemWeightsChange`) proving that changing item weights inside `EPIC` never shifts the overall `EPIC` drop rate[cite: 2, 4].