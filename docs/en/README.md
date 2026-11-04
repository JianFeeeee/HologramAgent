# docs/en — English documentation index

> **Status: index (current).** Established 2026-11-04.
>
> **中文索引**: [`../zh/README.md`](../zh/README.md)

---

## Why this directory exists

The repository's documentation grew Chinese-first, but this is a **public** repository
and `README.md` / `README_EN.md` are already bilingual. That asymmetry meant an
English-speaking reader could read the front page and then hit a wall at `docs/`.

This directory is the English side. It does **not** mirror `docs/zh/` file-for-file —
see "Translation policy" below.

---

## Translation policy

Not everything is worth translating, and pretending otherwise produces stale
translations that are worse than none.

| Document class | Translated? | Why |
| --- | --- | --- |
| **Lessons** (`engineering-lessons`) | ✅ Full | Highest reuse value; the failure shapes recur across the codebase |
| **Runbooks** (`deploy-`, `ci-cd-`, `site-infra-`) | ✅ Full | Operators who cannot read Chinese cannot deploy |
| **Index** (this file, `../zh/README.md`) | ✅ Full | Entry point; without it nothing else is discoverable |
| **Design docs** (`*-design.md`) | ⚠️ Summary only | Full translation of ~7000 lines would rot faster than it could be maintained. Each gets an English **summary + status line**, with a pointer to the Chinese original as the authority |
| **Historical snapshots / evidence** | ❌ No | Time-stamped by nature; translation adds no value |
| **Unimplemented plans** | ❌ No | Will change; see `docs/zh/README.md` §一 for why plans are not kept anyway |

**Rule of thumb**: if a document's content changes faster than a translation can be
reviewed, translate its *summary* instead of its body.

---

## Document classes (same taxonomy as `docs/zh/`)

| Class | Answers | Naming |
| --- | --- | --- |
| **Design** | Why is it built this way? | `*-design.md` |
| **Runbook** | How do I do it? | `*-runbook.md` |
| **Lessons** | What went wrong before? | `engineering-lessons.md` |
| **Open work** | What is not done yet? | `future-plan.md` |

Plans (`*-plan.md`, `*-checklist.md`) are **not** kept: once executed they read as
"what we intended" while being read as "what is". See `docs/zh/README.md` for the
full rationale.

---

## English documents

| Document | Class | Status |
| --- | --- | --- |
| [`engineering-lessons.md`](./engineering-lessons.md) | Lessons | ✅ Translation of the criteria-shape sections (§1, §5) |
| [`design-summaries.md`](./design-summaries.md) | Design | ✅ One-page summary per design doc |

## Chinese-only documents (with English summary available)

| Document | Class | English summary |
| --- | --- | --- |
| `docs/zh/kernel-modularization-design.md` | Design | [`design-summaries.md`](./design-summaries.md#kernel-modularization) |
| `docs/zh/kernel-secret-firewall-design.md` | Design | [`design-summaries.md`](./design-summaries.md#secret-firewall) |
| `docs/zh/input-scheduler-design.md` | Design | [`design-summaries.md`](./design-summaries.md#input-scheduler) |
| `docs/zh/resident-subagent-design.md` | Design | [`design-summaries.md`](./design-summaries.md#resident-subagent) |
| `docs/zh/toolcall-contract-and-sequence-design.md` | Design | [`design-summaries.md`](./design-summaries.md#toolcall-contract) |
| `docs/zh/context-overflow-l4-design.md` | Design | [`design-summaries.md`](./design-summaries.md#context-overflow-l4) |
| `docs/zh/commondev-design.md` | Design | plugin-side, Chinese only |
| `docs/zh/commondev-agent-runtime-design.md` | Design | plugin-side, Chinese only |
| `docs/zh/deploy-runbook.md` | Runbook | Chinese only — deploy targets are local hosts |
| `docs/zh/ci-cd-runbook.md` | Runbook | Chinese only — see `.github/workflows/` for the authoritative steps |

---

## How to keep this honest

Two rules, both learned from `docs/zh/README.md`'s own history:

1. **Every document carries a status line.** Allowed values: `current`,
   `implementation record`, `archive (historical)`, `plan (not implemented)`.
   A document without one is assumed stale.
2. **This index must be updated in the same commit** that adds, removes, or
   reclassifies a document. An index that lags is worse than no index — it teaches
   readers to trust the wrong map.
