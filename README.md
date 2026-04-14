# rocketchat-mcp

MCP server for [Rocket.Chat](https://rocket.chat). 28 tools for channels, messages, threads, DMs, files, users, reactions, and more.

Built for AI agents (Claude Code, Cursor, etc.) to interact with Rocket.Chat workspaces.

## Quick start

```bash
docker run -d -p 9886:8000 \
  -e ROCKETCHAT_URL=https://chat.example.com \
  -e ROCKETCHAT_AUTH_TOKEN=your-token \
  -e ROCKETCHAT_USER_ID=your-user-id \
  ghcr.io/enyonee/rocketchat-mcp:latest
```

Get your token and User ID: **Rocket.Chat -> Profile -> Personal Access Tokens**.

### Connect to Claude Code

Add to your MCP settings:

```json
{
  "mcpServers": {
    "rocketchat": {
      "url": "http://localhost:9886/mcp"
    }
  }
}
```

## Environment variables

| Variable | Required | Description |
|----------|----------|-------------|
| `ROCKETCHAT_URL` | Yes | Rocket.Chat server URL |
| `ROCKETCHAT_AUTH_TOKEN` | Yes | Personal access token |
| `ROCKETCHAT_USER_ID` | Yes | User ID |
| `READ_ONLY` | No | Set `true` to block all write operations |
| `MCP_PORT` | No | Server port (default: 8000) |

## Tools

### Channels

| Tool | Description |
|------|-------------|
| `list_channels` | List public channels, optionally filter by name |
| `list_joined_channels` | List channels the authenticated user has joined |
| `get_channel_info` | Get channel details by name or ID |
| `list_groups` | List private groups |
| `get_channel_members` | List members of a channel |

### Messages

| Tool | Description |
|------|-------------|
| `get_channel_messages` | Get messages with grouping, thread expansion, and compact mode |
| `search_messages` | Search messages by text in a channel |
| `get_message_context` | Get messages surrounding a specific message ID |
| `send_message` | Send a message to a channel |
| `edit_message` | Edit an existing message |
| `delete_message` | Delete a message |

### Threads

| Tool | Description |
|------|-------------|
| `get_thread_messages` | Get all replies in a thread |
| `reply_to_thread` | Reply to an existing thread |

### Direct Messages

| Tool | Description |
|------|-------------|
| `list_dms` | List DM conversations |
| `send_dm` | Send a direct message to a user |

### Files

| Tool | Description |
|------|-------------|
| `list_room_files` | List files shared in a channel |
| `download_file` | Download a file (images returned as visual content) |
| `send_file` | Upload a file to a channel (base64 input) |

### Users

| Tool | Description |
|------|-------------|
| `list_users` | List users, optionally filter by name |
| `get_user_info` | Get user details by username |

### Reactions & Pins

| Tool | Description |
|------|-------------|
| `add_reaction` | Add an emoji reaction to a message |
| `pin_message` | Pin a message |
| `unpin_message` | Unpin a message |
| `get_pinned_messages` | Get pinned messages in a channel |

### Analytics

| Tool | Description |
|------|-------------|
| `get_channel_digest` | Channel summary: active users, threads, files for the last N hours |
| `get_mentions` | Messages mentioning the authenticated user |
| `get_unread_channels` | Channels with unread messages, sorted by count |
| `mark_as_read` | Mark all messages in a channel as read |

## Key features

### Message grouping

Consecutive messages from the same user within 60 seconds are automatically merged. A text message followed by an image upload becomes a single grouped message:

```json
{
  "grouped": true,
  "ids": ["msg1", "msg2"],
  "texts": ["Check this out", ""],
  "files": [{"name": "screenshot.png", "url": "...", "type": "image/png"}],
  "user": "alice"
}
```

Disable with `group=false`.

### Thread expansion

Set `expand_threads=true` on `get_channel_messages` to inline thread replies:

```json
{
  "id": "parent_msg_id",
  "text": "Let's discuss this",
  "thread_replies": 5,
  "thread": [
    {"id": "reply1", "user": "bob", "text": "I agree"},
    {"id": "reply2", "user": "alice", "text": "Done"}
  ]
}
```

### Compact mode

Set `compact=true` on any message-returning tool to get minimal output (saves tokens):

```json
{"id": "abc", "user": "alice", "text": "Message text truncated to 200 chars...", "ts": "..."}
```

### Read-only mode

Set `READ_ONLY=true` to block: send_message, send_dm, reply_to_thread, add_reaction, send_file, edit_message, delete_message, pin_message, unpin_message.

## Development

```bash
# Run locally
ROCKETCHAT_URL=https://chat.example.com \
ROCKETCHAT_AUTH_TOKEN=token \
ROCKETCHAT_USER_ID=uid \
go run .

# Run tests (requires a live Rocket.Chat server)
ROCKETCHAT_URL=https://chat.example.com \
ROCKETCHAT_AUTH_TOKEN=token \
ROCKETCHAT_USER_ID=uid \
go test -v ./...

# Build Docker image
docker build -t rocketchat-mcp .
```

## License

MIT
