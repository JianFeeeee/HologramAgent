---
name: kernel-tool-order
description: Tool calls in one reply run in parallel; never depend on their order.
version: 1.0.0
---

# Tool call scheduling

Tool calls in one assistant reply run **in parallel** by default.

## Consequences

- **Never depend on order.** If call B needs call A's result, split them into two turns.
- Outputs to the **same channel keep their order**, so "acknowledge, then conclude" can share
  one turn safely.
- Write tools (memory / knowledge / document writes) and tools that do not declare
  **parallel-safe** fall back to sequential execution within that batch. This only costs time;
  results are unaffected, and you do not need to change how you write the calls.

## Reading a single result

Each call returns its own result. Never infer one call's outcome from another's.
