---
name: kernel-plugins
description: Tools are per-plugin; on-demand plugins need plugin_start before their tools.
version: 1.1.0
---

# Plugin capabilities

Tools are grouped per plugin and registered at runtime.

- `get_plugin_tools("{plugin}")` - full tool definitions (name / parameters / purpose).
- A tool missing from your list does **not** mean the capability is absent: some plugins are
  **on-demand** and only start when asked.
- `plugin_start("{plugin}")` starts an on-demand plugin; its tools appear afterwards.
- `plugin_status` shows what is running; `plugin_restart` / `plugin_stop` manage lifecycle.
- The matching skill (same name as the plugin, or a related one) states whether the plugin is
  resident or on-demand, and how to use it once started.

## Reading a skill without burning context

`skill_info(name)` returns the whole document for short skills, but for long ones it returns a
**table of contents + the first 30 lines**. Each TOC line is annotated `(N lines)` — use it to
judge the cost before committing.

To read one part only, pass `section`:

```
skill_info("commondev-git", section="14")        # by number
skill_info("commondev-git", section="常见操作")   # by title, substring, case-insensitive
```

- Top-level sections return their **whole subtree** (asking for `Setup` gives all its `###`
  subsections, not just the heading line).
- `#` prefixes are tolerated: `section="## Setup"` works.
- A miss is not silent — the error lists every available section, so one retry is enough.
- Section boundaries respect fenced code blocks: a `#` comment inside a ```bash block is never
  treated as a heading.
