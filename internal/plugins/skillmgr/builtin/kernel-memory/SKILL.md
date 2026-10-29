---
name: kernel-memory
description: Where past turns and facts live; read before recalling old context.
version: 1.0.0
---

# Memory layers

Four stores, one rule each:

- **Conversation timeline (L1)** - recent turns. Already in your context; just read it.
- **History (L2)** - older turns that left the window. Their full text still exists.
- **Graph memory (L3)** - facts and relations: who is what, the current value of something.
- **Knowledge** - written, reusable documents.

## Retrieving

| Need | Tool |
|---|---|
| A past episode in full | `coldcontext_query` |
| A fact or relation | `stablemem_recall` |
| Written reference material | `knowledge_search` |

### `coldcontext_query` semantics (read this before using it)

It does **not** return a summary you can reason from directly. It **re-inserts the matched
events into your conversation timeline at their original timestamps**. After the call,
read that section of the timeline; the return value only lists time / source / opening line.

Retrieval is **consuming**: a document served from L2 is removed from L2. If you need it
again later, it lives on in the timeline until pruning moves it back.

## Writing

- Facts worth keeping: `stablemem_commit` (triples).
- Written knowledge: `knowledge_write`.
- Do not commit transient chatter, tool output, or your own status lines.
