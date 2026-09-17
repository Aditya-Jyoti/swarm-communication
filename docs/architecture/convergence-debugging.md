---
title: Convergence Debugging
---

# Convergence Debugging

This page has been merged into
[Failure Detection and Failover](./failure-detection-and-failover#lessons-learned).

The short version: nodes elect the same leaders only if they hold the same membership table.
When two nodes disagree on the leaders, compare their tables, not the election.

```bash
curl -s 127.0.0.1:8080/api/state | jq -r '.nodes[] | .id as $n
  | .peers[] | select(.id=="node-2") | "\($n) seq=\(.seq) score=\(.score)"'
```

- Different `seq` on different nodes: a newer update is still spreading. Wait.
- Same `seq` but different values: a bug in how records are merged.
- Every node leads itself: the mesh is broken, not the election. See
  [The Mesh and the Handshake](./mesh-and-handshake).
