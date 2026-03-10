# rocketchat-mcp

MCP server for Rocket.Chat.

14 tools: channels, messages, threads, DMs, reactions, search, users, files.

## Quick start

```bash
docker build -t rocketchat-mcp .

docker run -d -p 9886:8000 \
  -e ROCKETCHAT_URL=https://chat.example.com \
  -e ROCKETCHAT_AUTH_TOKEN=your-token \
  -e ROCKETCHAT_USER_ID=your-user-id \
  rocketchat-mcp
```

Get token and User ID: Rocket.Chat → Profile → Personal Access Tokens.

## Connect to Claude Code

```json
{
  "mcpServers": {
    "rocketchat": {
      "url": "http://localhost:9886/mcp"
    }
  }
}
```

## Tools

| Group | Tools |
|-------|-------|
| Channels | `list_channels` `list_joined_channels` `get_channel_info` `list_groups` |
| Messages | `get_channel_messages` `search_messages` `send_message` `send_dm` |
| Threads | `get_thread_messages` `reply_to_thread` |
| Reactions | `add_reaction` |
| Users | `list_users` `get_user_info` |
| Files | `list_room_files` |

## License

MIT
