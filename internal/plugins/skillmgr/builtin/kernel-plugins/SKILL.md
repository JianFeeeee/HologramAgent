---
name: kernel-plugins
description: Tools are per-plugin; on-demand plugins need plugin_start before their tools.
version: 1.0.0
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
