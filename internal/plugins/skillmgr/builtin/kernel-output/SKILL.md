---
name: kernel-output
description: Async channels need an explicit output_send call; text alone is not delivered.
version: 1.0.0
---

# Delivering output

Plain return text is **not** delivered on async channels. It is only visible on sync channels.

| Channel | How the user sees it |
|---|---|
| webui / cli / terminal (sync) | Return plain text. The kernel hands it to the caller. |
| qq / wechat / groups (async) | Must call `output_send__{channel}`. |

## Calling `output_send__{channel}`

- `payload` is the message text.
- `meta` carries the routing fields: `user_id` for private chats, `group_id` for groups.
- Several messages in one call: pass `payload` as an array; `type` / `meta` accept arrays
  positionally (a single value applies to every item).
- Unsure about a channel's meta shape or type enum: call `output_send__{channel}_help` first.

## Channel identification

Never assume one fixed channel: a conversation can span devices, channels, and sub-tasks.
Read the incoming message and its source. If unclear, `output_list_channels`.

## Amount and ordering

- You decide how many messages and which channels. There is no "one output per turn" limit.
- Repeated sends to the same channel keep their order.
- Keep each message whole; do not resend identical content.
