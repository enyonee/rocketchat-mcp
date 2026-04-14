package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const defaultCount = 50

var (
	baseURL    string
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
	baseURL = strings.TrimRight(os.Getenv("ROCKETCHAT_URL"), "/")
	apiBase = baseURL + "/api/v1"
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

	srv := &http.Server{Addr: ":" + port, Handler: mux}
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("rocketchat-mcp listening on :%s", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
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

const maxDownloadSize = 25 * 1024 * 1024 // 25 MB

func rcDownload(fileURL string) ([]byte, string, error) {
	if strings.HasPrefix(fileURL, "/") {
		fileURL = baseURL + fileURL
	}
	req, err := http.NewRequest("GET", fileURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Auth-Token", authToken)
	req.Header.Set("X-User-Id", authUserID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d downloading file", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i > 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxDownloadSize {
		return nil, "", fmt.Errorf("file exceeds 25 MB limit")
	}
	return data, ct, nil
}

func rcUpload(roomID, filename, mimeType string, fileData []byte, msg string) (map[string]any, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	if mimeType != "" {
		h.Set("Content-Type", mimeType)
	}
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, err
	}
	part.Write(fileData)
	if msg != "" {
		writer.WriteField("description", msg)
	}
	writer.Close()

	req, err := http.NewRequest("POST", apiBase+"/rooms.upload/"+roomID, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Auth-Token", authToken)
	req.Header.Set("X-User-Id", authUserID)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	json.Unmarshal(raw, &result)
	return result, nil
}

var roomCache sync.Map // name -> roomID

func resolveRoomID(channel string) (string, error) {
	if len(channel) > 15 && !strings.Contains(channel, " ") && !strings.Contains(channel, "#") {
		return channel, nil
	}
	name := strings.TrimLeft(channel, "#")
	if cached, ok := roomCache.Load(name); ok {
		return cached.(string), nil
	}
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
				roomCache.Store(name, id)
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("channel/group '%s' not found", channel)
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
				link := str(att, "title_link")
				if link == "" {
					link = str(att, "image_url")
				}
				entry := map[string]string{}
				if v := str(att, "title"); v != "" {
					entry["title"] = v
				}
				if v := str(att, "type"); v != "" {
					entry["type"] = v
				}
				if v := absURL(link); v != "" {
					entry["url"] = v
				}
				if v := absURL(str(att, "image_url")); v != "" {
					entry["image_url"] = v
				}
				if v := str(att, "description"); v != "" {
					entry["description"] = v
				}
				if len(entry) > 0 {
					out = append(out, entry)
				}
			}
		}
		if len(out) > 0 {
			result["attachments"] = out
		}
	}
	if f, ok := m["file"].(map[string]any); ok {
		fileURL := ""
		// Build download URL from file ID and name
		if fid, _ := f["_id"].(string); fid != "" {
			fileURL = baseURL + "/file-upload/" + fid + "/" + url.PathEscape(str(f, "name"))
		}
		result["file"] = map[string]any{
			"name": str(f, "name"),
			"type": str(f, "type"),
			"size": f["size"],
			"url":  fileURL,
		}
	}
	return result
}

// fmtMsgCompact strips a formatted (or grouped) message down to id/user/text/ts only.
func fmtMsgCompact(m map[string]any) map[string]any {
	out := map[string]any{}
	// Grouped messages use "ids" and "texts"
	if ids, ok := m["ids"]; ok {
		out["ids"] = ids
		out["user"] = m["user"]
		out["ts"] = m["ts"]
		if texts, ok := m["texts"].([]string); ok {
			for i, t := range texts {
				texts[i] = truncate(t, 200)
			}
			out["texts"] = texts
		}
		return out
	}
	// Single message
	out["id"] = m["id"]
	out["user"] = m["user"]
	out["ts"] = m["ts"]
	if text, _ := m["text"].(string); text != "" {
		out["text"] = truncate(text, 200)
	} else {
		out["text"] = ""
	}
	return out
}

func applyCompact(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = fmtMsgCompact(m)
	}
	return out
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

func fmtDM(dm map[string]any) map[string]any {
	lastMsg := ""
	if last, ok := dm["lastMessage"].(map[string]any); ok {
		lastMsg = truncate(str(last, "msg"), 100)
	}
	var usernames []string
	if uns, ok := dm["usernames"].([]any); ok {
		for _, u := range uns {
			if s, ok := u.(string); ok {
				usernames = append(usernames, s)
			}
		}
	}
	return map[string]any{
		"id":           dm["_id"],
		"usernames":    usernames,
		"last_message": lastMsg,
		"msgs":         num(dm, "msgs"),
		"updated_at":   dm["_updatedAt"],
	}
}

// ── Хелперы ──────────────────────────────────────

func str(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func num(m map[string]any, key string) int    { v, _ := m[key].(float64); return int(v) }

func absURL(u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "/") {
		return baseURL + u
	}
	return u
}

// groupMessages merges consecutive messages from the same user within windowSec seconds.
// Combines text (with newlines) and collects all attachments/files.
func groupMessages(msgs []map[string]any) []map[string]any {
	if len(msgs) == 0 {
		return msgs
	}
	const windowSec = 60
	parseTS := func(v any) time.Time {
		switch t := v.(type) {
		case string:
			ts, _ := time.Parse(time.RFC3339Nano, t)
			return ts
		}
		return time.Time{}
	}
	var grouped []map[string]any
	var cur map[string]any // nil or grouped-format message
	for _, m := range msgs {
		if cur == nil {
			cur = m // not yet grouped, single message
			continue
		}
		curUser := str(cur, "user")
		t1, t2 := parseTS(cur["ts"]), parseTS(m["ts"])
		sameUser := curUser == str(m, "user")
		withinWindow := !t1.IsZero() && !t2.IsZero() && abs64(t1.Sub(t2).Seconds()) < windowSec
		if sameUser && withinWindow {
			// Need to group - convert cur to grouped format if not already
			if _, isGrouped := cur["ids"]; !isGrouped {
				cur = toGrouped(cur)
			}
			mergeInto(cur, m)
		} else {
			grouped = append(grouped, cur)
			cur = m
		}
	}
	if cur != nil {
		grouped = append(grouped, cur)
	}
	return grouped
}

func abs64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// toGrouped converts a single message into grouped format with arrays.
func toGrouped(m map[string]any) map[string]any {
	g := map[string]any{
		"ids":     []string{fmt.Sprint(m["id"])},
		"user":    m["user"],
		"ts":      m["ts"],
		"grouped": true,
	}
	if text, _ := m["text"].(string); text != "" {
		g["texts"] = []string{text}
	} else {
		g["texts"] = []string{}
	}
	if atts := m["attachments"]; atts != nil {
		g["attachments"] = atts
	}
	if f := m["file"]; f != nil {
		g["files"] = []any{f}
	} else {
		g["files"] = []any{}
	}
	if v := m["thread_id"]; v != nil {
		g["thread_id"] = v
	}
	if v := m["thread_replies"]; v != nil {
		g["thread_replies"] = v
	}
	if v := m["reactions"]; v != nil {
		g["reactions"] = v
	}
	return g
}

func mergeInto(dst, src map[string]any) {
	// IDs
	ids, _ := dst["ids"].([]string)
	ids = append(ids, fmt.Sprint(src["id"]))
	dst["ids"] = ids

	// Texts
	texts, _ := dst["texts"].([]string)
	if srcText, _ := src["text"].(string); srcText != "" {
		texts = append(texts, srcText)
	}
	dst["texts"] = texts

	// Attachments
	if srcAtt := src["attachments"]; srcAtt != nil {
		dstAtt, _ := dst["attachments"].([]map[string]string)
		srcSlice, _ := srcAtt.([]map[string]string)
		dst["attachments"] = append(dstAtt, srcSlice...)
	}

	// Files
	if srcFile := src["file"]; srcFile != nil {
		files, _ := dst["files"].([]any)
		dst["files"] = append(files, srcFile)
	}

	// Thread info: keep first non-nil
	if src["thread_id"] != nil && dst["thread_id"] == nil {
		dst["thread_id"] = src["thread_id"]
	}
	if src["thread_replies"] != nil && dst["thread_replies"] == nil {
		dst["thread_replies"] = src["thread_replies"]
	}

	// Reactions: merge
	if srcR, ok := src["reactions"].(map[string]any); ok {
		dstR, _ := dst["reactions"].(map[string]any)
		if dstR == nil {
			dstR = map[string]any{}
		}
		for k, v := range srcR {
			dstR[k] = v
		}
		dst["reactions"] = dstR
	}
}

// expandThreads fetches thread replies for messages that have threads (parallel).
func expandThreads(msgs []map[string]any, roomID string) {
	type threadResult struct {
		idx     int
		replies []map[string]any
	}
	var indices []int
	for i, m := range msgs {
		tc, _ := m["thread_replies"].(int)
		if tc > 0 {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		return
	}
	ch := make(chan threadResult, len(indices))
	for _, i := range indices {
		go func(idx int) {
			m := msgs[idx]
			var msgID string
			if ids, ok := m["ids"].([]string); ok && len(ids) > 0 {
				msgID = ids[0]
			} else if id, ok := m["id"].(string); ok {
				msgID = id
			}
			if msgID == "" {
				ch <- threadResult{idx, nil}
				return
			}
			data, err := rcGet("/chat.getThreadMessages", url.Values{"tmid": {msgID}, "count": {"50"}})
			if err != nil {
				ch <- threadResult{idx, nil}
				return
			}
			ch <- threadResult{idx, fmtAll(getSlice(data, "messages"), fmtMsg)}
		}(i)
	}
	for range indices {
		r := <-ch
		if r.replies != nil {
			msgs[r.idx]["thread"] = r.replies
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func getArgs(r mcp.CallToolRequest) map[string]any {
	m, _ := r.Params.Arguments.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func sarg(r mcp.CallToolRequest, k string) string { v, _ := getArgs(r)[k].(string); return v }
func barg(r mcp.CallToolRequest, k string) bool  { v, _ := getArgs(r)[k].(bool); return v }

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

	// ── Channels ──

	s.AddTool(mcp.NewTool("list_channels",
		mcp.WithDescription("List public channels. Use query to filter by name. Returns id, name, type, member count, message count, topic, and last message preview."),
		mcp.WithNumber("count", mcp.Description("Number of results (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithString("query", mcp.Description("Filter channels by name substring")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var extra url.Values
		if q := sarg(req, "query"); q != "" {
			extra = url.Values{"query": {q}}
		}
		return listEndpoint(req, "/channels.list", "channels", 100, fmtChannel, extra)
	})

	s.AddTool(mcp.NewTool("list_joined_channels",
		mcp.WithDescription("List channels the authenticated user has joined. Returns the same format as list_channels."),
		mcp.WithNumber("count", mcp.Description("Number of results (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return listEndpoint(req, "/channels.list.joined", "channels", 100, fmtChannel, nil)
	})

	s.AddTool(mcp.NewTool("get_channel_info",
		mcp.WithDescription("Get detailed information about a channel by name or ID. Returns id, name, type, member count, message count, topic, and last message preview."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
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
		mcp.WithDescription("List private groups the authenticated user belongs to. Returns the same format as list_channels."),
		mcp.WithNumber("count", mcp.Description("Number of results (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return listEndpoint(req, "/groups.listAll", "groups", 100, fmtChannel, nil)
	})

	s.AddTool(mcp.NewTool("get_channel_members",
		mcp.WithDescription("List members of a channel or group. Returns id, username, name, status, and roles for each member."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("count", mcp.Description("Number of results (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, 100)
		p.Set("roomId", roomID)
		data, err := rcGet("/channels.members", p)
		if err != nil {
			return fail(err.Error())
		}
		members := fmtAll(getSlice(data, "members"), fmtUser)
		return res(map[string]any{"channel": ch, "room_id": roomID, "total": num(data, "total"), "members": members})
	})

	// ── Messages ──

	s.AddTool(mcp.NewTool("get_channel_messages",
		mcp.WithDescription("Get messages from a channel (newest first). group=true (default) merges consecutive messages from the same user within 60s. expand_threads=true inlines thread replies. compact=true returns only id/user/text/ts."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("count", mcp.Description("Number of messages (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithBoolean("group", mcp.Description("Group consecutive messages from same user within 60s (default true)")),
		mcp.WithBoolean("expand_threads", mcp.Description("Inline thread replies into each message (default false)")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
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
		// Group by default unless explicitly false
		args := getArgs(req)
		if _, explicit := args["group"]; !explicit || barg(req, "group") {
			msgs = groupMessages(msgs)
		}
		// Expand threads
		if barg(req, "expand_threads") {
			expandThreads(msgs, roomID)
		}
		if barg(req, "compact") {
			msgs = applyCompact(msgs)
		}
		return res(map[string]any{"channel": ch, "room_id": roomID, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("search_messages",
		mcp.WithDescription("Search messages in a channel by text. Returns matching messages with full context. compact=true returns only id/user/text/ts."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithString("search_text", mcp.Required(), mcp.Description("Text to search for")),
		mcp.WithNumber("count", mcp.Description("Number of results (default 50)")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
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
		if barg(req, "compact") {
			msgs = applyCompact(msgs)
		}
		return res(map[string]any{"channel": ch, "room_id": roomID, "search_text": search, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Send a message to a channel or group (by name without # or room ID). Blocked in read-only mode."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or room ID")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message text (supports Markdown)")),
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
		mcp.WithDescription("Send a direct message to a user (username without @). Creates the DM conversation if it does not exist. Blocked in read-only mode."),
		mcp.WithString("username", mcp.Required(), mcp.Description("Recipient username (without @)")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message text")),
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
			return fail(fmt.Sprintf("failed to create DM with %s", username))
		}
		roomID, _ := room["_id"].(string)
		if roomID == "" {
			return fail(fmt.Sprintf("failed to create DM with %s", username))
		}
		data, err := rcPost("/chat.sendMessage", map[string]any{"message": map[string]string{"rid": roomID, "msg": sarg(req, "text")}})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "sent", "id": msg["_id"], "to": username, "room_id": roomID, "ts": msg["ts"]})
	})

	// ── Threads ──

	s.AddTool(mcp.NewTool("get_thread_messages",
		mcp.WithDescription("Get messages from a thread by its parent message ID. Returns all replies in the thread. compact=true returns only id/user/text/ts."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("Parent message ID that started the thread")),
		mcp.WithNumber("count", mcp.Description("Number of messages (default 50)")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tid := sarg(req, "thread_id")
		p := paging(req, defaultCount)
		p.Set("tmid", tid)
		data, err := rcGet("/chat.getThreadMessages", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		if barg(req, "compact") {
			msgs = applyCompact(msgs)
		}
		return res(map[string]any{"thread_id": tid, "count": len(msgs), "messages": msgs})
	})

	s.AddTool(mcp.NewTool("reply_to_thread",
		mcp.WithDescription("Reply to a thread. If channel is omitted, it is resolved automatically from the parent message. Blocked in read-only mode."),
		mcp.WithString("thread_id", mcp.Required(), mcp.Description("Parent message ID")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Reply text")),
		mcp.WithString("channel", mcp.Description("Room ID (optional, auto-resolved if omitted)")),
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
			return fail("could not determine thread channel")
		}
		data, err := rcPost("/chat.sendMessage", map[string]any{"message": map[string]string{"rid": ch, "msg": sarg(req, "text"), "tmid": tid}})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "sent", "id": msg["_id"], "thread_id": tid, "ts": msg["ts"]})
	})

	// ── Reactions ──

	s.AddTool(mcp.NewTool("add_reaction",
		mcp.WithDescription("Add an emoji reaction to a message. Use standard emoji codes like :thumbsup:, :white_check_mark:, :eyes:. Blocked in read-only mode."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID to react to")),
		mcp.WithString("emoji", mcp.Required(), mcp.Description("Emoji code with colons (e.g. :thumbsup:)")),
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

	// ── Users ──

	s.AddTool(mcp.NewTool("list_users",
		mcp.WithDescription("List users on the server. Use query to filter by name or username. Returns id, username, name, status, and roles."),
		mcp.WithNumber("count", mcp.Description("Number of results (default 100)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithString("query", mcp.Description("Filter by name or username substring")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var extra url.Values
		if q := sarg(req, "query"); q != "" {
			extra = url.Values{"query": {q}}
		}
		return listEndpoint(req, "/users.list", "users", 100, fmtUser, extra)
	})

	s.AddTool(mcp.NewTool("get_user_info",
		mcp.WithDescription("Get detailed information about a user by username. Returns id, username, display name, online status, and roles."),
		mcp.WithString("username", mcp.Required(), mcp.Description("Username to look up")),
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

	// ── Files ──

	s.AddTool(mcp.NewTool("list_room_files",
		mcp.WithDescription("List files shared in a channel or group. Returns id, name, type, size, uploader, and download URL for each file."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("count", mcp.Description("Number of results (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
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
		return res(map[string]any{"channel": ch, "room_id": roomID, "total": num(data, "total"), "files": fmtAll(getSlice(data, "files"), fmtFile)})
	})

	s.AddTool(mcp.NewTool("download_file",
		mcp.WithDescription("Download a file from Rocket.Chat by URL (from list_room_files or message attachments). Images returned as visual content, text as plain text, other files as base64."),
		mcp.WithString("url", mcp.Required(), mcp.Description("File URL (relative /file-upload/... or absolute)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fileURL := sarg(req, "url")
		if fileURL == "" {
			return fail("url is required")
		}
		data, mimeType, err := rcDownload(fileURL)
		if err != nil {
			return fail(err.Error())
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		if strings.HasPrefix(mimeType, "image/") {
			return mcp.NewToolResultImage(fmt.Sprintf("Image (%s, %d bytes)", mimeType, len(data)), encoded, mimeType), nil
		}
		if strings.HasPrefix(mimeType, "text/") {
			return mcp.NewToolResultText(string(data)), nil
		}
		return mcp.NewToolResultResource(
			fmt.Sprintf("File (%s, %d bytes)", mimeType, len(data)),
			mcp.BlobResourceContents{URI: fileURL, MIMEType: mimeType, Blob: encoded},
		), nil
	})

	s.AddTool(mcp.NewTool("send_file",
		mcp.WithDescription("Upload a file to a channel or group. File content must be base64-encoded. Blocked in read-only mode."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithString("content", mcp.Required(), mcp.Description("Base64-encoded file content")),
		mcp.WithString("filename", mcp.Required(), mcp.Description("Filename with extension (e.g. report.pdf)")),
		mcp.WithString("mime_type", mcp.Description("MIME type (auto-detected from extension if empty)")),
		mcp.WithString("message", mcp.Description("Optional message to accompany the file")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		raw, err := base64.StdEncoding.DecodeString(sarg(req, "content"))
		if err != nil {
			return fail("invalid base64: " + err.Error())
		}
		filename := sarg(req, "filename")
		mimeType := sarg(req, "mime_type")
		data, err := rcUpload(roomID, filename, mimeType, raw, sarg(req, "message"))
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "uploaded", "channel": ch, "room_id": roomID, "message": data["message"]})
	})

	// ── Message context ──

	s.AddTool(mcp.NewTool("get_message_context",
		mcp.WithDescription("Get messages surrounding a specific message ID - useful for understanding discussion context. Returns N messages before and after the target. compact=true returns only id/user/text/ts."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Target message ID")),
		mcp.WithNumber("before", mcp.Description("Number of messages before target (default 5)")),
		mcp.WithNumber("after", mcp.Description("Number of messages after target (default 5)")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		msgID := sarg(req, "message_id")
		before := iarg(req, "before", 5)
		after := iarg(req, "after", 5)
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		// Get target message timestamp
		msgData, err := rcGet("/chat.getMessage", url.Values{"msgId": {msgID}})
		if err != nil {
			return fail(err.Error())
		}
		msg, _ := msgData["message"].(map[string]any)
		if msg == nil {
			return fail("message not found")
		}
		ts, _ := msg["ts"].(string)
		if ts == "" {
			return fail("message has no timestamp")
		}
		// Fetch before + after in parallel
		total := before + after + 1
		fetchAfter := after + 1
		type histResult struct {
			msgs []map[string]any
		}
		chBefore := make(chan histResult, 1)
		chAfter := make(chan histResult, 1)
		go func() {
			data, _ := rcGet("/channels.history", url.Values{
				"roomId": {roomID}, "latest": {ts}, "count": {strconv.Itoa(before)},
			})
			chBefore <- histResult{fmtAll(getSlice(data, "messages"), fmtMsg)}
		}()
		go func() {
			data, _ := rcGet("/channels.history", url.Values{
				"roomId": {roomID}, "oldest": {ts}, "count": {strconv.Itoa(fetchAfter + 20)},
			})
			chAfter <- histResult{fmtAll(getSlice(data, "messages"), fmtMsg)}
		}()
		msgsBefore := (<-chBefore).msgs
		msgsAfterRaw := (<-chAfter).msgs
		// msgsAfterRaw is newest-first, we need oldest-first (closest to target)
		// Reverse to chronological, then take first fetchAfter items
		for i, j := 0, len(msgsAfterRaw)-1; i < j; i, j = i+1, j-1 {
			msgsAfterRaw[i], msgsAfterRaw[j] = msgsAfterRaw[j], msgsAfterRaw[i]
		}
		msgsAfter := msgsAfterRaw
		if len(msgsAfter) > fetchAfter {
			msgsAfter = msgsAfter[:fetchAfter]
		}

		// Build the target message
		targetFmt := fmtMsg(msg)
		targetFmt["is_target"] = true

		// Combine: before (reverse to chronological) + target + after (already chronological)
		all := make([]map[string]any, 0, total)
		for i := len(msgsBefore) - 1; i >= 0; i-- {
			all = append(all, msgsBefore[i])
		}
		all = append(all, targetFmt)
		// Filter out target from after-set (in case API includes it)
		for _, m := range msgsAfter {
			if m["id"] != msgID {
				all = append(all, m)
			}
		}
		if barg(req, "compact") {
			all = applyCompact(all)
		}
		return res(map[string]any{"channel": ch, "room_id": roomID, "target_id": msgID, "count": len(all), "messages": all})
	})

	s.AddTool(mcp.NewTool("get_pinned_messages",
		mcp.WithDescription("Get pinned messages in a channel. Returns full message details including attachments and reactions. compact=true returns only id/user/text/ts."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("count", mcp.Description("Number of results (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, defaultCount)
		p.Set("roomId", roomID)
		data, err := rcGet("/chat.getPinnedMessages", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		if barg(req, "compact") {
			msgs = applyCompact(msgs)
		}
		return res(map[string]any{"channel": ch, "room_id": roomID, "count": len(msgs), "messages": msgs})
	})

	// ── Unread ──

	s.AddTool(mcp.NewTool("get_unread_channels",
		mcp.WithDescription("Get channels with unread messages for the authenticated user. Sorted by unread count descending. Returns room_id, name, type, unread count, and mention counts."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Use epoch as updatedSince to get ALL subscriptions
		data, err := rcGet("/subscriptions.get", url.Values{"updatedSince": {"2000-01-01T00:00:00Z"}})
		if err != nil {
			return fail(err.Error())
		}
		subs := getSlice(data, "update")
		var unread []map[string]any
		for _, s := range subs {
			sub, ok := s.(map[string]any)
			if !ok {
				continue
			}
			u := num(sub, "unread")
			if u == 0 {
				continue
			}
			unread = append(unread, map[string]any{
				"room_id":        sub["rid"],
				"name":           str(sub, "name"),
				"type":           str(sub, "t"),
				"unread":         u,
				"user_mentions":  num(sub, "userMentions"),
				"group_mentions": num(sub, "groupMentions"),
			})
		}
		sort.Slice(unread, func(i, j int) bool {
			return num(unread[i], "unread") > num(unread[j], "unread")
		})
		return res(map[string]any{"total": len(unread), "channels": unread})
	})

	s.AddTool(mcp.NewTool("mark_as_read",
		mcp.WithDescription("Mark all messages in a channel as read. Clears the unread counter for the authenticated user."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		_, err = rcPost("/subscriptions.read", map[string]string{"rid": roomID})
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "marked_read", "channel": ch, "room_id": roomID})
	})

	s.AddTool(mcp.NewTool("get_mentions",
		mcp.WithDescription("Get messages that mention the authenticated user in a channel. Useful for finding messages that need your attention. compact=true returns only id/user/text/ts."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("count", mcp.Description("Number of results (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
		mcp.WithBoolean("compact", mcp.Description("Return only id, user, text (truncated to 200 chars), ts (default false)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		p := paging(req, defaultCount)
		p.Set("roomId", roomID)
		data, err := rcGet("/chat.getMentionedMessages", p)
		if err != nil {
			return fail(err.Error())
		}
		msgs := fmtAll(getSlice(data, "messages"), fmtMsg)
		if barg(req, "compact") {
			msgs = applyCompact(msgs)
		}
		return res(map[string]any{"channel": ch, "room_id": roomID, "count": len(msgs), "messages": msgs})
	})

	// ── DMs ──

	s.AddTool(mcp.NewTool("list_dms",
		mcp.WithDescription("List direct message conversations for the authenticated user. Returns id, participant usernames, last message preview, message count, and last update time."),
		mcp.WithNumber("count", mcp.Description("Number of results (default 50)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		p := paging(req, defaultCount)
		data, err := rcGet("/im.list", p)
		if err != nil {
			return fail(err.Error())
		}
		dms := fmtAll(getSlice(data, "ims"), fmtDM)
		return res(map[string]any{"total": num(data, "total"), "dms": dms})
	})

	// ── Digest ──

	s.AddTool(mcp.NewTool("get_channel_digest",
		mcp.WithDescription("Channel activity summary for the last N hours: active users with message counts, threads, shared files. Fetches up to 500 messages and aggregates server-side."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel name or ID")),
		mcp.WithNumber("hours", mcp.Description("Time window in hours (default 24)")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ch := sarg(req, "channel")
		hours := iarg(req, "hours", 24)
		roomID, err := resolveRoomID(ch)
		if err != nil {
			return fail(err.Error())
		}
		oldest := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
		p := url.Values{
			"roomId": {roomID},
			"oldest": {oldest},
			"count":  {"500"},
		}
		data, err := rcGet("/channels.history", p)
		if err != nil {
			return fail(err.Error())
		}
		messages := getSlice(data, "messages")

		// Aggregate
		userCounts := map[string]int{}
		var threads []map[string]any
		var files []map[string]any
		for _, m := range messages {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if u, ok := msg["u"].(map[string]any); ok {
				username, _ := u["username"].(string)
				userCounts[username]++
			}
			if tc, ok := msg["tcount"].(float64); ok && tc > 0 {
				threads = append(threads, map[string]any{
					"id":      msg["_id"],
					"text":    truncate(str(msg, "msg"), 120),
					"replies": int(tc),
				})
			}
			if f, ok := msg["file"].(map[string]any); ok {
				files = append(files, map[string]any{
					"name": str(f, "name"),
					"type": str(f, "type"),
				})
			}
		}
		// Sort users by message count
		type userCount struct {
			User  string `json:"user"`
			Count int    `json:"count"`
		}
		var users []userCount
		for u, c := range userCounts {
			users = append(users, userCount{u, c})
		}
		sort.Slice(users, func(i, j int) bool { return users[i].Count > users[j].Count })

		return res(map[string]any{
			"channel":        ch,
			"room_id":        roomID,
			"period_hours":   hours,
			"total_messages": len(messages),
			"active_users":   users,
			"threads":        threads,
			"files_shared":   files,
		})
	})

	// ── Message management ──

	s.AddTool(mcp.NewTool("edit_message",
		mcp.WithDescription("Edit an existing message by ID. Requires room_id and message_id. Blocked in read-only mode."),
		mcp.WithString("room_id", mcp.Required(), mcp.Description("Room ID where the message is")),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID to edit")),
		mcp.WithString("text", mcp.Required(), mcp.Description("New message text")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		data, err := rcPost("/chat.update", map[string]string{
			"roomId": sarg(req, "room_id"),
			"msgId":  sarg(req, "message_id"),
			"text":   sarg(req, "text"),
		})
		if err != nil {
			return fail(err.Error())
		}
		msg := respMsg(data)
		return res(map[string]any{"status": "edited", "id": msg["_id"], "ts": msg["ts"]})
	})

	s.AddTool(mcp.NewTool("delete_message",
		mcp.WithDescription("Delete a message by ID. Requires room_id and message_id. Blocked in read-only mode."),
		mcp.WithString("room_id", mcp.Required(), mcp.Description("Room ID where the message is")),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID to delete")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		_, err := rcPost("/chat.delete", map[string]string{
			"roomId": sarg(req, "room_id"),
			"msgId":  sarg(req, "message_id"),
		})
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "deleted", "id": sarg(req, "message_id")})
	})

	s.AddTool(mcp.NewTool("pin_message",
		mcp.WithDescription("Pin a message in its channel. The message will appear in get_pinned_messages. Blocked in read-only mode."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID to pin")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		_, err := rcPost("/chat.pinMessage", map[string]string{
			"messageId": sarg(req, "message_id"),
		})
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "pinned", "id": sarg(req, "message_id")})
	})

	s.AddTool(mcp.NewTool("unpin_message",
		mcp.WithDescription("Unpin a previously pinned message. Blocked in read-only mode."),
		mcp.WithString("message_id", mcp.Required(), mcp.Description("Message ID to unpin")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r, blocked := writeGuard(); blocked {
			return r, nil
		}
		_, err := rcPost("/chat.unPinMessage", map[string]string{
			"messageId": sarg(req, "message_id"),
		})
		if err != nil {
			return fail(err.Error())
		}
		return res(map[string]any{"status": "unpinned", "id": sarg(req, "message_id")})
	})
}
