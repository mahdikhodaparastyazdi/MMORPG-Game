-- migrations/001_init.sql

-- 1. Pre-defined Players
CREATE TABLE IF NOT EXISTS players (
    id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(100) NOT NULL UNIQUE,
    character_class VARCHAR(50) NOT NULL,
    gold_balance BIGINT NOT NULL DEFAULT 0 CHECK (gold_balance >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. Item Catalog (Single source of truth for Item Type & Rarity)
CREATE TABLE IF NOT EXISTS item_catalog (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(120) NOT NULL,
    item_type VARCHAR(50) NOT NULL,
    rarity VARCHAR(32) NOT NULL CHECK (rarity IN ('COMMON', 'RARE', 'EPIC', 'LEGENDARY')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 3. Versioned & Immutable Loot Configurations
CREATE TABLE IF NOT EXISTS loot_configs (
    id UUID PRIMARY KEY,
    dungeon_id VARCHAR(64) NOT NULL,
    difficulty INT NOT NULL,
    version INT NOT NULL,
    allocation_mode VARCHAR(32) NOT NULL CHECK (allocation_mode IN ('PARTY_POOL', 'PERSONAL_LOOT')),
    contribution_mode VARCHAR(32) NOT NULL CHECK (contribution_mode IN ('INDEPENDENT', 'CONTRIBUTION_AWARE')),
    party_pool_drop_count INT NOT NULL DEFAULT 4 CHECK (party_pool_drop_count > 0),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (dungeon_id, difficulty, version)
);

-- Overall Drop Probability per Rarity Tier for a specific Loot Config version
CREATE TABLE IF NOT EXISTS loot_config_rarities (
    loot_config_id UUID NOT NULL REFERENCES loot_configs(id) ON DELETE CASCADE,
    rarity VARCHAR(32) NOT NULL CHECK (rarity IN ('COMMON', 'RARE', 'EPIC', 'LEGENDARY')),
    drop_probability DOUBLE PRECISION NOT NULL CHECK (drop_probability >= 0.0 AND drop_probability <= 1.0),
    PRIMARY KEY (loot_config_id, rarity)
);

-- Eligible Items & Relative Weights (References item_catalog without redefining Rarity)
CREATE TABLE IF NOT EXISTS loot_config_items (
    loot_config_id UUID NOT NULL REFERENCES loot_configs(id) ON DELETE CASCADE,
    item_id VARCHAR(64) NOT NULL REFERENCES item_catalog(id),
    weight INT NOT NULL CHECK (weight > 0),
    PRIMARY KEY (loot_config_id, item_id)
);

-- 4. Parties & Party Members
CREATE TABLE IF NOT EXISTS parties (
    id UUID PRIMARY KEY,
    join_code VARCHAR(16) NOT NULL UNIQUE,
    leader_id VARCHAR(64) NOT NULL REFERENCES players(id),
    status VARCHAR(32) NOT NULL DEFAULT 'FORMING' CHECK (status IN ('FORMING', 'IN_DUNGEON', 'COMPLETED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS party_members (
    party_id UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    player_id VARCHAR(64) NOT NULL REFERENCES players(id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (party_id, player_id)
);

-- 5. Dungeon Runs & Locked Eligible Participants
CREATE TABLE IF NOT EXISTS dungeon_runs (
    id UUID PRIMARY KEY,
    party_id UUID NOT NULL REFERENCES parties(id),
    dungeon_id VARCHAR(64) NOT NULL,
    difficulty INT NOT NULL,
    loot_config_id UUID NOT NULL REFERENCES loot_configs(id),
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SETTLED_SUCCESS', 'SETTLED_FAILED')),
    rng_seed BIGINT,
    exchange_expires_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS run_participants (
    run_id UUID NOT NULL REFERENCES dungeon_runs(id) ON DELETE CASCADE,
    player_id VARCHAR(64) NOT NULL REFERENCES players(id),
    contribution DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    effective_weight DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    PRIMARY KEY (run_id, player_id)
);

-- 6. Concrete Item Instances (Tracks Provenance & Current Ownership)
CREATE TABLE IF NOT EXISTS item_instances (
    id UUID PRIMARY KEY,
    item_id VARCHAR(64) NOT NULL REFERENCES item_catalog(id),
    origin_run_id UUID NOT NULL REFERENCES dungeon_runs(id),
    settlement_slot INT NOT NULL,
    initial_owner_id VARCHAR(64) NOT NULL REFERENCES players(id),
    current_owner_id VARCHAR(64) NOT NULL REFERENCES players(id),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (origin_run_id, settlement_slot)
);

-- 7. Post-Dungeon Exchange Requests
-- Rule: Max 1 Purchase Request per Participant per Run enforced via UNIQUE (run_id, buyer_id)
CREATE TABLE IF NOT EXISTS exchange_requests (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES dungeon_runs(id),
    buyer_id VARCHAR(64) NOT NULL REFERENCES players(id),
    target_item_id VARCHAR(64) NOT NULL REFERENCES item_catalog(id),
    offered_gold BIGINT NOT NULL CHECK (offered_gold > 0),
    status VARCHAR(32) NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'COMPLETED', 'EXPIRED', 'CANCELLED')),
    fulfilled_by_seller_id VARCHAR(64) REFERENCES players(id),
    fulfilled_item_instance_id UUID REFERENCES item_instances(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    UNIQUE (run_id, buyer_id)
);

-- 8. Audit & Operator Support Tables
CREATE TABLE IF NOT EXISTS settlement_audit_logs (
    run_id UUID PRIMARY KEY REFERENCES dungeon_runs(id),
    loot_config_id UUID NOT NULL REFERENCES loot_configs(id),
    rng_seed BIGINT NOT NULL,
    roll_details JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS economy_ledger (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES dungeon_runs(id),
    event_type VARCHAR(64) NOT NULL CHECK (event_type IN ('DUNGEON_REWARD_GRANT', 'EXCHANGE_SETTLEMENT')),
    exchange_request_id UUID REFERENCES exchange_requests(id),
    item_instance_id UUID REFERENCES item_instances(id),
    from_player_id VARCHAR(64) REFERENCES players(id),
    to_player_id VARCHAR(64) NOT NULL REFERENCES players(id),
    gold_transferred BIGINT NOT NULL DEFAULT 0,
    buyer_gold_before BIGINT,
    buyer_gold_after BIGINT,
    seller_gold_before BIGINT,
    seller_gold_after BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- SEED DATA: Pre-made Players, Item Catalog, and Default Loot Configurations
-- ============================================================================

INSERT INTO players (id, username, character_class, gold_balance) VALUES
    ('player-a', 'Arthas (Player A)', 'Paladin', 1500),
    ('player-b', 'Sylvanas (Player B)', 'Ranger', 1000),
    ('player-c', 'Jaina (Player C)', 'Mage', 1200),
    ('player-d', 'Thrall (Player D)', 'Shaman', 900),
    ('player-e', 'Illidan (Player E)', 'Demon Hunter', 2000),
    ('player-f', 'Tyrande (Player F)', 'Priest', 1100),
    ('player-g', 'Garrosh (Player G - Outsider)', 'Warrior', 5000)
ON CONFLICT (id) DO NOTHING;

INSERT INTO item_catalog (id, name, item_type, rarity) VALUES
    ('item-iron-sword', 'Iron Broadsword', 'WEAPON', 'COMMON'),
    ('item-leather-boots', 'Scout Leather Boots', 'ARMOR', 'COMMON'),
    ('item-sapphire-ring', 'Sapphire Ring of Wisdom', 'ACCESSORY', 'RARE'),
    ('item-runed-shield', 'Runed Mithril Shield', 'ARMOR', 'RARE'),
    ('item-shadow-blade', 'Shadowfang Doomblade', 'WEAPON', 'EPIC'),
    ('item-void-mantle', 'Mantle of the Void', 'ARMOR', 'EPIC'),
    ('item-dragon-crown', 'Crown of the Eternal Dragon', 'HELMET', 'LEGENDARY')
ON CONFLICT (id) DO NOTHING;

-- Seed Config 1: Dungeon X (Shadowfang Keep), Difficulty 18 (Matches Use Case in prompt)
INSERT INTO loot_configs (id, dungeon_id, difficulty, version, allocation_mode, contribution_mode, party_pool_drop_count, is_active)
VALUES ('11111111-1111-1111-1111-111111111111', 'dungeon-x', 18, 1, 'PARTY_POOL', 'CONTRIBUTION_AWARE', 4, TRUE)
ON CONFLICT (dungeon_id, difficulty, version) DO NOTHING;

INSERT INTO loot_config_rarities (loot_config_id, rarity, drop_probability) VALUES
    ('11111111-1111-1111-1111-111111111111', 'COMMON', 0.45),
    ('11111111-1111-1111-1111-111111111111', 'RARE', 0.30),
    ('11111111-1111-1111-1111-111111111111', 'EPIC', 0.20),
    ('11111111-1111-1111-1111-111111111111', 'LEGENDARY', 0.05)
ON CONFLICT DO NOTHING;

INSERT INTO loot_config_items (loot_config_id, item_id, weight) VALUES
    ('11111111-1111-1111-1111-111111111111', 'item-iron-sword', 60),
    ('11111111-1111-1111-1111-111111111111', 'item-leather-boots', 40),
    ('11111111-1111-1111-1111-111111111111', 'item-sapphire-ring', 50),
    ('11111111-1111-1111-1111-111111111111', 'item-runed-shield', 50),
    ('11111111-1111-1111-1111-111111111111', 'item-shadow-blade', 70),
    ('11111111-1111-1111-1111-111111111111', 'item-void-mantle', 30),
    ('11111111-1111-1111-1111-111111111111', 'item-dragon-crown', 100)
ON CONFLICT DO NOTHING;

-- Seed Config 2: Dungeon Y (Molten Citadel), Difficulty 10 (Personal Loot + Contribution Independent)
INSERT INTO loot_configs (id, dungeon_id, difficulty, version, allocation_mode, contribution_mode, party_pool_drop_count, is_active)
VALUES ('22222222-2222-2222-2222-222222222222', 'dungeon-y', 10, 1, 'PERSONAL_LOOT', 'INDEPENDENT', 4, TRUE)
ON CONFLICT (dungeon_id, difficulty, version) DO NOTHING;

INSERT INTO loot_config_rarities (loot_config_id, rarity, drop_probability) VALUES
    ('22222222-2222-2222-2222-222222222222', 'COMMON', 0.50),
    ('22222222-2222-2222-2222-222222222222', 'RARE', 0.35),
    ('22222222-2222-2222-2222-222222222222', 'EPIC', 0.15),
    ('22222222-2222-2222-2222-222222222222', 'LEGENDARY', 0.00)
ON CONFLICT DO NOTHING;

INSERT INTO loot_config_items (loot_config_id, item_id, weight) VALUES
    ('22222222-2222-2222-2222-222222222222', 'item-iron-sword', 100),
    ('22222222-2222-2222-2222-222222222222', 'item-sapphire-ring', 100),
    ('22222222-2222-2222-2222-222222222222', 'item-shadow-blade', 100)
ON CONFLICT DO NOTHING;