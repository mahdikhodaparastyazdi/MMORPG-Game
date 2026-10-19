# MMORPG Dungeon Reward & Loot Settlement Service

A production-oriented Go + PostgreSQL Modular Monolith implementing the **Dungeon Reward & Loot Settlement** and **Post-Dungeon Exchange** vertical slice for an MMORPG backend.

## Architecture & Documentation
* **[`docs/architecture.md`](docs/architecture.md)**: High-level MMORPG Online Services architecture (50k CCU / 1M MAU), subsystem boundaries, state ownership, and detailed trade-offs.
* **[`docs/final-notes.md`](docs/final-notes.md)**: Post-implementation architectural reflections, explicit assumptions made for open requirements, and future production enhancements.
* **[`AI_USAGE.md`](AI_USAGE.md)**: Transparency log of AI tool usage, workflow integration, and architectural/code refinements made over initial suggestions.

---

## Quick Start (Clean Checkout to Running System)

### Docker Compose
Run the entire stack (PostgreSQL 16 + automated schema migrations & seed data + Go Backend + Web UI) with a single command:

```bash
docker compose up --build
