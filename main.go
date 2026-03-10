package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const defaultCount = 50

var (
	apiBase    string
	authToken  string
	authUserID string
	readOnly   bool
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		port := envOr("MCP_PORT", "8000")
		resp, err := http.Get("http://localhost:" + port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	for _, v := range []string{"ROCKETCHAT_URL", "ROCKETCHAT_AUTH_TOKEN", "ROCKETCHAT_USER_ID"} {
		if os.Getenv(v) == "" {
			log.Fatalf("%s is required", v)
		}
	}
	apiBase = strings.TrimRight(os.Getenv("ROCKETCHAT_URL"), "/") + "/api/v1"
	authToken = os.Getenv("ROCKETCHAT_AUTH_TOKEN")
	authUserID = os.Getenv("ROCKETCHAT_USER_ID")
	readOnly = strings.EqualFold(os.Getenv("READ_ONLY"), "true")

	port := envOr("MCP_PORT", "8000")

	s := server.NewMCPServer("rocketchat", "1.0.0")
	registerTools(s)

	streamable := server.NewStreamableHTTPServer(s)
	sseServer := server.NewSSEServer(s)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	})
	mux.Handle("/mcp", streamable)
	mux.Handle("/", sseServer)

	log.Printf("rocketchat-mcp listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── RC API ────────────────────────────────────────

func rcRequest(method, endpoint string, body any) (map[string]any, error) {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, apiBase+endpoint, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", authToken)
	req.Header.Set("X-User-Id", authUserID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 401:
		return nil, fmt.Errorf("unauthorized - check ROCKETCHAT_AUTH_TOKEN / ROCKETCHAT_USER_ID")
	case 403:
		return nil, fmt.Errorf("forbidden: %s", endpoint)
	case 404:
		return nil, fmt.Errorf("not found: %s", endpoint)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, endpoint)
	}

	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON from %s", endpoint)
	}
	return result, nil
}

func rcGet(endpoint string, params url.Values) (map[string]any, error) {
	path := endpoint
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return rcRequest("GET", path, nil)
}

func rcPost(endpoint string, body any) (map[string]any, error) {
	return rcRequest("POST", endpoint, body)
}

func resolveRoomID(channel string) (string, error) {
	if len(channel) > 15 && !strings.Contains(channel, " ") && !strings.Contains(channel, "#") {
		return channel, nil
	}
	name := strings.TrimLeft(channel, "#")
	for _, ep := range []struct{ path, key string }{
		{"/channels.info", "channel"},
		{"/groups.info", "group"},
	} {
		data, err := rcGet(ep.path, url.Values{"roomName": {name}})
		if err != nil {
			continue
		}
		if obj, ok := data[ep.key].(map[string]any); ok {
			if id, ok := obj["_id"].(string); ok {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("канал/группа '%s' не найден(а)", channel)
}

// ── Форматирование ───────────────────────────────

func fmtMsg(m map[string]any) map[string]any {
	user := ""
	if u, ok := m["u"].(map[string]any); ok {
		user, _ = u["username"].(string)
	}
	result := map[string]any{
		"id": m["_id"], "text": str(m, "msg"),
		"user": user, "ts": m["ts"],
	}
	if tmid, _ := m["tmid"].(string); tmid != "" {
		result["thread_id"] = tmid
	}
	if tc, ok := m["tcount"].(float64); ok && tc > 0 {
		result["thread_replies"] = int(tc)
	}
	if reactions, ok := m["reactions"].(map[string]any); ok {
		r := map[string]any{}
		for k, v := range reactions {
			if rv, ok := v.(map[string]any); ok {
				r[k] = rv["usernames"]
			}
		}
		result["reactions"] = r
	}
	if atts, ok := m["attachments"].([]any); ok {
		var out []map[string]string
		for _, a := range atts {
			if att, ok := a.(map[string]any); ok {
				out = append(out, map[string]string{
					"title": str(att, "title"), "type": str(att, "type"),
					"url": str(att, "title_link"),
				})
			}
		}
		result["attachments"] = out
	}
	if f, ok := m["file"].(map[string]any); ok {
		result["file"] = map[string]string{"name": str(f, "name"), "type": str(f, "type")}
	}
	return result
}

func fmtChannel(ch map[string]any) map[string]any {
	name := str(ch, "name")
	if name == "" {
		name = str(ch, "fname")
	}
	lastMsg := ""
	if last, ok := ch["lastMessage"].(map[string]any); ok {
		lastMsg = str(last, "msg")
		if len(lastMsg) > 100 {
			lastMsg = lastMsg[:100]
		}
	}
	return map[string]any{
		"id": ch["_id"], "name": name, "type": str(ch, "t"),
		"members": num(ch, "usersCount"), "msgs": num(ch, "msgs"),
		"topic": str(ch, "topic"), "last_message": lastMsg,
	}
}

func fmtUser(u map[string]any) map[string]any {
	return map[string]any{
		"id": u["_id"], "username": str(u, "username"),
		"name": str(u, "name"), "status": str(u, "status"),
		"roles": u["roles"],
	}
}

func fmtFile(f map[string]any) map[string]any {
	user := ""
	if u, ok := f["user"].(map[string]any); ok {
		user, _ = u["username"].(string)
	}
	return map[string]any{
		"id": f["_id"], "name": str(f, "name"), "type": str(f, "type"),
		"size": num(f, "size"), "user": user, "url": str(f, "url"),
	}
}

// ── Хелперы ──────────────────────────────────────

func str(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func num(m map[string]any, key string) int    { v, _ := m[key].(float64); return int(v) }

func getArgs(r mcp.CallToolRequest) map[string]any {
	m, _ := r.Params.Arguments.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func sarg(r mcp.CallToolRequest, k string) string { v, _ := getArgs(r)[k].(string); return v }

func iarg(r mcp.CallToolRequest, k string, def int) int {
	v, ok := getArgs(r)[k].(float64)
	if !ok {
		return def
	}
	return int(v)
}

func paging(req mcp.CallToolRequest, defCount int) url.Values {
	return url.Values{
		"count":  {strconv.Itoa(iarg(req, "count", defCount))},
		"offset": {strconv.Itoa(iarg(req, "offset", 0))},
	}
}

func res(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

func fail(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(msg), nil
}

func writeGuard() (*mcp.CallToolResult, bool) {
	if readOnly {
		return mcp.NewToolResultError("read-only mode: write operations are disabled"), true
	}
	return nil, false
}

func getSlice(data map[string]any, key string) []any { v, _ := data[key].([]any); return v }

func fmtAll(items []any, fn func(map[string]any) map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, fn(m))
		}
	}
	return out
}

// listEndpoint - общий паттерн для list_channels, list_joined_channels, list_groups, list_users
func listEndpoint(req mcp.CallToolRequest, endpoint, key string, defCount int, formatter func(map[string]any) map[string]any, extra url.Values) (*mcp.CallToolResult, error) {
	p := paging(req, defCount)
	for k, v := range extra {
		p[k] = v
	}
	data, err := rcGet(endpoint, p)
	if err != nil {
		return fail(err.Error())
	}
	return res(map[string]any{"total": num(data, "total"), key: fmtAll(getSlice(data, key), formatter)})
}

// respMsg - извлечь message из ответа POST (send_message, send_dm, reply_to_thread)
func respMsg(data map[string]any) map[string]any {
	msg, _ := data["message"].(map[string]any)
	if msg == nil {
		return map[string]any{}
	}
	return msg
}

// ── Tools ────────────────────────────────────────

func registerTools(s *server.MCPServer) {

	// ── Каналы ──

	s.AddTool(mcp.NewTool("list_channels",
		mcp.WithDescription("Список публичных каналов. query - фильтр по имени."),
		mcp.WithNumber("count", mcp.Description("Количество (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
		mcp.WithString("query", mcp.Description("Фильтр по имени")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var extra url.Values
		if q := sarg(req, "query"); q != "" {
			extra = url.Values{"query": {q}}
		}
		return listEndpoint(req, "/channels.list", "channels", 100, fmtChannel, extra)
	})

	s.AddTool(mcp.NewTool("list_joined_channels",
		mcp.WithDescription("Каналы текущего пользователя."),
		mcp.WithNumber("count", mcp.Description("Количество (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return listEndpoint(req, "/channels.list.joined", "channels", 100, fmtChannel, nil)
	})

	s.AddTool(mcp.NewTool("get_channel_info",
		mcp.WithDescription("Информация о канале по имени или ID."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Имя или ID канала")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		data, err := rcGet("/channels.info", url.Values{"roomName": {ch}})
		if err != nil {
			data, err = rcGet("/channels.info", url.Values{"roomId": {ch}})
			if err != nil {
				return fail(err.Error())
			}
		}
		if c, ok := data["channel"].(map[string]any); ok {
			return res(fmtChannel(c))
		}
		return res(map[string]any{})
	})

	s.AddTool(mcp.NewTool("list_groups",
		mcp.WithDescription("Приватные группы текущего пользователя."),
		mcp.WithNumber("count", mcp.Description("Количество (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return listEndpoint(req, "/groups.listAll", "groups", 100, fmtChannel, nil)
	})

	// ── Сообщения ──

	s.AddTool(mcp.NewTool("get_channel_messages",
		mcp.WithDescription("Сообщения из канала/группы (от новых к старым)."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Имя или ID канала")),
		mcp.WithNumber("count", mcp.Description("Количество (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, defaultCount)
		p.Set("roomId", roomID)
		data, err := rcGet("/channels.history", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		return res(map[string]any{"channel": ch, "room_id": roomID, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("search_messages",
		mcp.WithDescription("Поиск сообщений в канале по тексту."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Имя или ID канала")),
		mcp.WithString("search_text", mcp.Required(), mcp.Description("Текст для поиска")),
		mcp.WithNumber("count", mcp.Description("Количество (default 50)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		search := sarg(req, "search_text")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, defaultCount)
		p.Set("roomId", roomID)
		p.Set("searchText", search)
		data, err := rcGet("/chat.search", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		return res(map[string]any{"channel": ch, "search_text": search, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Отправить сообщение в канал/группу (по имени без # или room ID)."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Канал")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Текст сообщения")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		ch := sarg(req, "channel")
		data, err := rcPost("/chat.postMessage", map[string]string{"channel": ch, "text": sarg(req, "text")})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "sent", "id": msg["_id"], "channel": ch, "ts": msg["ts"]})
	})

	s.AddTool(mcp.NewTool("send_dm",
		mcp.WithDescription("Личное сообщение пользователю (username без @)."),
		mcp.WithString("username", mcp.Required(), mcp.Description("Username получателя")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Текст сообщения")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		username := sarg(req, "username")
		dm, err := rcPost("/im.create", map[string]string{"username": username})
		if err != nil {
			return fail(err.Error())
		}
		room, _ := dm["room"].(map[string]any)
		if room == nil {
			return fail(fmt.Sprintf("не удалось создать DM с %s", username))
		}
		roomID, _ := room["_id"].(string)
		if roomID == "" {
			return fail(fmt.Sprintf("не удалось создать DM с %s", username))
		}
		data, err := rcPost("/chat.sendMessage", map[string]any{"message": map[string]string{"rid": roomID, "msg": sarg(req, "text")}})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "sent", "id": msg["_id"], "to": username, "room_id": roomID, "ts": msg["ts"]})
	})

	// ── Треды ──

	s.AddTool(mcp.NewTool("get_thread_messages",
		mcp.WithDescription("Сообщения из треда (thread_id = ID родительского сообщения)."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("ID родительского сообщения")),
		mcp.WithNumber("count", mcp.Description("Количество (default 50)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tid := sarg(req, "thread_id")
		p := paging(req, defaultCount)
		p.Set("tmid", tid)
		data, err := rcGet("/chat.getThreadMessages", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		return res(map[string]any{"thread_id": tid, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("reply_to_thread",
		mcp.WithDescription("Ответить в тред. channel - room ID (если не указан, определится автоматически)."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("ID родительского сообщения")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Текст ответа")),
		mcp.WithString("channel", mcp.Description("Room ID (опционально)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		tid := sarg(req, "thread_id")
		ch := sarg(req, "channel")
		if ch == "" {
			parent, err := rcGet("/chat.getMessage", url.Values{"msgId": {tid}})
			if err != nil {
				return fail(err.Error())
			}
			if m, ok := parent["message"].(map[string]any); ok {
				ch, _ = m["rid"].(string)
			}
		}
		if ch == "" {
			return fail("не удалось определить канал треда")
		}
		data, err := rcPost("/chat.sendMessage", map[string]any{"message": map[string]string{"rid": ch, "msg": sarg(req, "text"), "tmid": tid}})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "sent", "id": msg["_id"], "thread_id": tid, "ts": msg["ts"]})
	})

	// ── Реакции ──

	s.AddTool(mcp.NewTool("add_reaction",
		mcp.WithDescription("Реакция на сообщение (emoji: \":thumbsup:\", \":white_check_mark:\" и т.д.)."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("ID сообщения")),
		mcp.WithString("emoji", mcp.Required(), mcp.Description("Эмодзи")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		msgID := sarg(req, "message_id")
		emoji := sarg(req, "emoji")
		_, err := rcPost("/chat.react", map[string]string{"messageId": msgID, "emoji": emoji})
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "reacted", "message_id": msgID, "emoji": emoji})
	})

	// ── Пользователи ──

	s.AddTool(mcp.NewTool("list_users",
		mcp.WithDescription("Список пользователей. query - фильтр по имени/username."),
		mcp.WithNumber("count", mcp.Description("Количество (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
		mcp.WithString("query", mcp.Description("Фильтр")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var extra url.Values
		if q := sarg(req, "query"); q != "" {
			extra = url.Values{"query": {q}}
		}
		return listEndpoint(req, "/users.list", "users", 100, fmtUser, extra)
	})

	s.AddTool(mcp.NewTool("get_user_info",
		mcp.WithDescription("Информация о пользователе по username."),
		mcp.WithString("username", mcp.Required(), mcp.Description("Username")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		data, err := rcGet("/users.info", url.Values{"username": {sarg(req, "username")}})
		if err != nil {
			return fail(err.Error())
		}
		if u, ok := data["user"].(map[string]any); ok {
			return res(fmtUser(u))
		}
		return res(map[string]any{})
	})

	// ── Файлы ──

	s.AddTool(mcp.NewTool("list_room_files",
		mcp.WithDescription("Файлы в канале/группе."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Имя или ID канала")),
		mcp.WithNumber("count", mcp.Description("Количество (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Смещение")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, defaultCount)
		p.Set("roomId", roomID)
		data, err := rcGet("/channels.files", p)
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"channel": ch, "total": num(data, "total"), "files": fmtAll(getSlice(data, "files"), fmtFile)})
	})
}
