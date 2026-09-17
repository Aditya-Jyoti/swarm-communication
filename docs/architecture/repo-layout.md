---
title: Repository Layout
---

# Repository Layout

This page has been merged into the
[System Overview](./overview#repository-layout).

In short:

| Path | What it holds |
|---|---|
| `backend/` | Go module: `cmd/`, `pkg/` (including `pkg/geo`, the drone latency model), `Dockerfile`, `deploy/` |
| `frontend/` | Dashboard (3D airspace, grouped view, message animation, simulation panel) and the nginx config |
| `blender/` | Scene generator, Blender add-on and its tests |
| `docs/` | This site |
| `docker-compose.yml` | The local swarm |
| `.env.example` | Every tunable |
| `scripts/e2e.sh` | End-to-end test: self-healing, chaos kill, sim changes |
