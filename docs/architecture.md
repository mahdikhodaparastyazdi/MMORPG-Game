# MMORPG Online Services & Loot Settlement Architecture

## 1. High-Level System Overview & Scale Targets

This document outlines the backend architecture for a large-scale Online Multiplayer RPG supporting **50,000 Concurrent Users (CCU)** and **1,000,000 Monthly Active Users (MAU)**, with a deep dive into the **Dungeon Reward & Loot Settlement** subsystem.

### Scale & Traffic Estimates (50k CCU)
* **Active Dungeon Instances:** Assuming 4 players per party and ~60% of CCU inside instanced dungeons, ~7,500 concurrent dungeon runs are active at peak.
* **Dungeon Completion Rate:** With an average dungeon duration of 15 minutes, the system processes ~8–15 Dungeon Completion settlements per second on average, with bursts up to **50–100 settlements/sec** during peak event hours.
* **Read/Write Profile:** Player authentication, session heartbeats, and social/party states are high-throughput, low-latency operations (handled via Redis + Edge Gateways). Inventory mutations, reward settlements, and economic exchanges are lower-throughput but require **strict ACID guarantees, zero data loss, and complete auditability** (handled via PostgreSQL).

---

## 2. Global Architecture Diagram

```mermaid
graph TD
    subgraph Client Tier
        GC[Game Client]
        OP[Operator / Admin Portal]
    end

    subgraph Dedicated Game Hosting Fleet
        GS[Gameplay Server Fleet<br/>Combat, Physics, Dungeon Sim]
    end

    subgraph Edge & Routing
        AGW[Client API Gateway<br/>TLS, JWT Auth, Rate Limiting]
        IGW[Internal Server Gateway<br/>mTLS / HMAC Auth]
    end

    subgraph Online Services Layer - Go Microservices
        AUTH[Auth & Session Service]
        CHAR[Character & Profile Service]
        SOC[Party, Matchmaking & Social Service]
        LOOT[Dungeon & Loot Settlement Service<br/>*Vertical Slice Focus*]
        ECON[Inventory & Economy Service<br/>*Vertical Slice Focus*]
        ADM[Admin, Audit & Config Service]
    end

    subgraph Data & Messaging Infrastructure
        PG[(PostgreSQL Cluster<br/>Primary + Read Replicas)]
        REDIS[(Redis Cluster<br/>Sessions, Presence, Ephemeral Party)]
        NATS[[Message Bus - NATS JetStream / Kafka<br/>Async Events, Notifications, Audit Logs]]
    end

    GC -->|UDP / WebRTC<br/>60Hz State Sync| GS
    GC -->|HTTPS / WebSockets| AGW
    OP -->|HTTPS + RBAC| AGW
    GS -->|gRPC / HTTPS<br/>Server-Authoritative Calls| IGW

    AGW --> AUTH
    AGW --> CHAR
    AGW --> SOC
    AGW --> LOOT
    AGW --> ECON
    AGW --> ADM

    IGW --> LOOT
    IGW --> SOC

    AUTH --> REDIS
    AUTH --> PG
    SOC --> REDIS
    SOC --> PG
    CHAR --> PG
    LOOT --> PG
    LOOT --> ECON
    ECON --> PG
    ADM --> PG

    LOOT -.->|Publish Settlement Events| NATS
    ECON -.->|Publish Trade & Ledger Events| NATS