---
name: kernel-terminal
description: Terminal waits for input; it is NOT cmd_run. Read before using vim/ssh/top.
version: 1.0.0
---

# Terminals vs one-shot commands

Two different tools. Picking the wrong one looks like it works, then fails in a way that
points somewhere else.

| Tool | Use for | Lifetime |
| --- | --- | --- |
| `cmd_run` | **One** command, then done: queries, scripts, `cd && … && grep …`, pipes, redirects, heredocs | Process exits when the command exits |
| `terminal_create` | A program that **keeps running and keeps asking for input**: `vim`, `ssh`, `top`, `less`, an interactive REPL | Stays open until you `terminal_close` or its `timeout` expires |

## The failure this prevents

Running `terminal_create` with a compound one-shot command
(`cd /x && echo … && ls … && grep …`) creates a terminal that exits the moment the command
finishes. Reading it then fails with `read /dev/ptmx: input/output error`. The next instinct is
to create another terminal for the next command — and now every "command" costs a terminal,
fills the terminal list, and each one dies mid-read.

**If the command runs to completion on its own, it belongs in `cmd_run`.** A terminal is only
justified when the thing you are talking to is *still running and waiting for you*.

## Driving a terminal

`terminal_create` returns an ID. From then on the terminal is **silent by default** — it does
not push output at you.

1. `terminal_read(id)` — read what is on screen now. Do this after acting, not on a timer.
2. `terminal_write(id, input=..., key=...)` — send text and/or a key (`Enter`, `Escape`, `C-c`)
   in **one call**. `input` without `key` does not submit; the program just sits there.
3. `terminal_close(id)` — release it. Terminals default to a 5-minute `timeout`; leaving them
   open wastes a slot.

`notify` is opt-in. Without it you get nothing. Add it only when you genuinely want a wake-up
(`exit` for "tell me when it finished", `buffer=8192` for "tell me when there is a lot of
unread output"). Watching a fast-scrolling program with `interval` produces noise, not signal.

## Sending keystrokes

`key` is for **control**, `input` is for **text**:

| Want | Send |
| --- | --- |
| Submit a command line | `input="ls -la"`, `key="Enter"` |
| Interrupt a running program | `key="C-c"` |
| Leave a pager or editor without saving | `key="Escape"`, then `key="q"` for pagers |
| Quit `vim` without saving | `key="Escape"`, then `input=":q!"`, `key="Enter"` |

## Reading output that scrolled past

`terminal_read` returns what is currently visible plus recent scrollback. If the output you need
scrolled out of range, do not blindly re-read in a loop — either grow `rows` at create time, or
for one-shot inspection prefer `cmd_run`, whose result is returned whole.

## Checklist before reaching for a terminal

- Does the command finish by itself? → `cmd_run`.
- Does it need me to type at it more than once? → `terminal_create`.
- Am I about to create a second terminal for "the next command"? → stop; the first one was
  the wrong tool.
