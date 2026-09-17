---
title: Repository Layout
---

# Repository Layout

This page has been merged into the
[System Overview](./overview#repository-layout).

In short:

| Path | What it holds |
|---|---|
| `backend/` | Go module: `cmd/`, `pkg/`, `Dockerfile`, `deploy/` |
| `frontend/` | Dashboard files and the nginx config |
| `docs/` | This site |
| `docker-compose.yml` | The local swarm |
| `.env.example` | Every tunable |
| `scripts/e2e.sh` | End-to-end self-healing test |
