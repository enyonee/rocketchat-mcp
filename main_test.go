package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// ── Test infra ──────────────────────────────────────

var (
	testPort    string
	testSession string
	testCh      string // resolved channel name for tests
	testRoomID  string // resolved room ID
)

func TestMain(m *testing.M) {
	for _, v := range []string{"ROCKETCHAT_URL", "ROCKETCHAT_AUTH_TOKEN", "ROCKETCHAT_USER_ID"} {
		if os.Getenv(v) == "" {
			fmt.Fprintf(os.Stderr, "SKIP: %s not set\n", v)
			os.Exit(0)
		}
	}
	baseURL = strings.TrimRight(os.Getenv("ROCKETCHAT_URL"), "/")
	apiBase = baseURL + "/api/v1"
	authToken = os.Getenv("ROCKETCHAT_AUTH_TOKEN")
	authUserID = os.Getenv("ROCKETCHAT_USER_ID")

	// Find free port & start server
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	testPort = fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	s := server.NewMCPServer("rocketchat", "1.0.0")
	registerTools(s)
	mux := http.NewServeMux()
	mux.Handle("/mcp", server.NewStreamableHTTPServer(s))
	go http.ListenAndServe("127.0.0.1:"+testPort, mux)
	time.Sleep(300 * time.Millisecond)

	// Init MCP session
	resp := mcpRaw("initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1.0"},
	})
	resp.Body.Close()
	testSession = resp.Header.Get("Mcp-Session-Id")
	if testSession == "" {
		fmt.Fprintf(os.Stderr, "FAIL: no session\n")
		os.Exit(1)
	}

	// Test channel - ONLY the dedicated test channel, never real work channels
	testCh = "test"
	if testRoomID == "" {
		rid, err := resolveRoomID(testCh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: resolve %s: %v\n", testCh, err)
			os.Exit(1)
		}
		testRoomID = rid
	}
	fmt.Fprintf(os.Stderr, "Test channel: %s (room_id: %s)\n", testCh, testRoomID)

	os.Exit(m.Run())
}

func mcpRaw(method string, params any) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+testPort+"/mcp",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if testSession != "" {
		req.Header.Set("Mcp-Session-Id", testSession)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

type rpcResponse struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct{ Message string } `json:"error"`
}

func callToolOnce(name string, args map[string]any) (*rpcResponse, error) {
	resp := mcpRaw("tools/call", map[string]any{"name": name, "arguments": args})
	defer resp.Body.Close()
	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, err
	}
	return &rpc, nil
}

func callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	// Retry up to 4 times on 429 with exponential backoff
	var rpc *rpcResponse
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<attempt) * time.Second // 2s, 4s, 8s
			time.Sleep(delay)
		}
		var err error
		rpc, err = callToolOnce(name, args)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rpc.Error != nil {
			t.Fatalf("RPC error: %s", rpc.Error.Message)
		}
		if rpc.Result.IsError {
			errText := rpc.Result.Content[0].Text
			if strings.Contains(errText, "429") {
				if attempt < 3 {
					t.Logf("429 on %s, retry %d", name, attempt+1)
					continue
				}
				t.Skipf("persistent 429 on %s (RC rate limit)", name)
			}
			t.Fatalf("tool error: %s", errText)
		}
		break
	}
	for _, c := range rpc.Result.Content {
		if c.Type == "text" {
			var m map[string]any
			if err := json.Unmarshal([]byte(c.Text), &m); err != nil {
				t.Fatalf("parse JSON: %v", err)
			}
			return m
		}
	}
	t.Fatal("no text content")
	return nil
}

func callToolRaw(t *testing.T, name string, args map[string]any) []map[string]any {
	t.Helper()
	type rawRPC struct {
		Result struct {
			Content []map[string]any `json:"content"`
			IsError bool             `json:"isError"`
		} `json:"result"`
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
		resp := mcpRaw("tools/call", map[string]any{"name": name, "arguments": args})
		var rpc rawRPC
		json.NewDecoder(resp.Body).Decode(&rpc)
		resp.Body.Close()
		if rpc.Result.IsError {
			if attempt < 2 {
				// Check for 429 in content
				for _, c := range rpc.Result.Content {
					if text, _ := c["text"].(string); strings.Contains(text, "429") {
						t.Logf("429 on %s, retry %d", name, attempt+1)
						continue
					}
				}
			}
			t.Fatalf("tool error: %v", rpc.Result.Content)
		}
		return rpc.Result.Content
	}
	return nil
}

func requireStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("expected string at %q, got %T (%v)", key, m[key], m[key])
	}
	return v
}

func requireSlice(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("expected array at %q, got %T", key, m[key])
	}
	return v
}

func requireMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", v)
	}
	return m
}

// ── Channels ──────────────────────────────────

func TestListChannels(t *testing.T) {
	r := callTool(t, "list_channels", map[string]any{"count": 3})
	channels := requireSlice(t, r, "channels")
	if len(channels) == 0 {
		t.Fatal("no channels")
	}
	ch := requireMap(t, channels[0])
	requireStr(t, ch, "id")
	requireStr(t, ch, "name")
}

func TestListJoinedChannels(t *testing.T) {
	r := callTool(t, "list_joined_channels", map[string]any{"count": 3})
	channels := requireSlice(t, r, "channels")
	if len(channels) == 0 {
		t.Fatal("no joined channels")
	}
}

func TestGetChannelInfo(t *testing.T) {
	r := callTool(t, "get_channel_info", map[string]any{"channel": testCh})
	requireStr(t, r, "id")
	requireStr(t, r, "name")
}

func TestGetChannelMembers(t *testing.T) {
	r := callTool(t, "get_channel_members", map[string]any{"channel": testRoomID, "count": 3})
	members := requireSlice(t, r, "members")
	if len(members) == 0 {
		t.Fatal("no members")
	}
	m := requireMap(t, members[0])
	requireStr(t, m, "username")
	requireStr(t, m, "name")
}

// ── Messages ──────────────────────────────────

func TestGetChannelMessages(t *testing.T) {
	r := callTool(t, "get_channel_messages", map[string]any{"channel": testRoomID, "count": 5})
	requireStr(t, r, "channel")
	requireStr(t, r, "room_id")
	msgs := requireSlice(t, r, "messages")
	if len(msgs) == 0 {
		t.Skip("test channel empty (write tests will populate it)")
	}
	m := requireMap(t, msgs[0])
	if _, hasID := m["id"]; !hasID {
		if _, hasIDs := m["ids"]; !hasIDs {
			t.Fatal("message has neither id nor ids")
		}
	}
}

func TestGroupingStructure(t *testing.T) {
	r := callTool(t, "get_channel_messages", map[string]any{"channel": testRoomID, "count": 50})
	msgs := requireSlice(t, r, "messages")

	for _, raw := range msgs {
		m := requireMap(t, raw)
		if grouped, _ := m["grouped"].(bool); grouped {
			// Grouped: ids, texts as arrays
			ids := requireSlice(t, m, "ids")
			if len(ids) < 2 {
				t.Error("grouped should have >= 2 ids")
			}
			for _, id := range ids {
				if _, ok := id.(string); !ok {
					t.Errorf("ids[] should be string, got %T", id)
				}
			}
			texts := requireSlice(t, m, "texts")
			for _, txt := range texts {
				if _, ok := txt.(string); !ok {
					t.Errorf("texts[] should be string, got %T", txt)
				}
			}
			requireSlice(t, m, "files")
			requireStr(t, m, "user")
		} else {
			// Single: id, text as strings
			requireStr(t, m, "id")
			if _, ok := m["text"].(string); !ok {
				t.Error("single message text should be string")
			}
			requireStr(t, m, "user")
		}
	}
}

func TestNoGrouping(t *testing.T) {
	// Send 2 quick messages to test channel, then verify no grouping with group=false
	ts := time.Now().Format("15:04:05")
	s1 := callTool(t, "send_message", map[string]any{"channel": testRoomID, "text": "[test] nogroup1 " + ts})
	s2 := callTool(t, "send_message", map[string]any{"channel": testRoomID, "text": "[test] nogroup2 " + ts})

	r := callTool(t, "get_channel_messages", map[string]any{
		"channel": testRoomID, "count": 5, "group": false,
	})
	msgs := requireSlice(t, r, "messages")
	for _, raw := range msgs {
		m := requireMap(t, raw)
		if _, hasIDs := m["ids"]; hasIDs {
			t.Fatal("group=false should not produce grouped messages")
		}
		requireStr(t, m, "id")
	}

	// Cleanup
	callTool(t, "delete_message", map[string]any{"room_id": testRoomID, "message_id": requireStr(t, s1, "id")})
	callTool(t, "delete_message", map[string]any{"room_id": testRoomID, "message_id": requireStr(t, s2, "id")})
}

func TestExpandThreads(t *testing.T) {
	r := callTool(t, "get_channel_messages", map[string]any{
		"channel": testRoomID, "count": 50, "expand_threads": true,
	})
	msgs := requireSlice(t, r, "messages")
	for _, raw := range msgs {
		m := requireMap(t, raw)
		if thread, ok := m["thread"].([]any); ok && len(thread) > 0 {
			reply := requireMap(t, thread[0])
			requireStr(t, reply, "id")
			requireStr(t, reply, "user")
			return
		}
	}
	t.Log("no threads found in test channel")
}

func TestSearchMessages(t *testing.T) {
	r := callTool(t, "search_messages", map[string]any{
		"channel": testRoomID, "search_text": "http", "count": 3,
	})
	requireStr(t, r, "channel")
	// search_text echoed back
	if st, _ := r["search_text"].(string); st != "http" {
		t.Errorf("expected search_text=http, got %s", st)
	}
}

func TestGetMessageContext(t *testing.T) {
	// Get a message to use as target
	list := callTool(t, "get_channel_messages", map[string]any{
		"channel": testRoomID, "count": 5, "group": false,
	})
	msgs := requireSlice(t, list, "messages")
	if len(msgs) == 0 {
		t.Skip("no messages")
	}
	msgID := requireStr(t, requireMap(t, msgs[0]), "id")

	r := callTool(t, "get_message_context", map[string]any{
		"channel": testRoomID, "message_id": msgID, "before": 2, "after": 2,
	})
	requireStr(t, r, "target_id")
	contextMsgs := requireSlice(t, r, "messages")

	found := false
	for _, raw := range contextMsgs {
		m := requireMap(t, raw)
		if isTarget, _ := m["is_target"].(bool); isTarget {
			found = true
			if mid, _ := m["id"].(string); mid != msgID {
				t.Errorf("target id mismatch: %s != %s", mid, msgID)
			}
		}
	}
	if !found {
		t.Error("target message not found (no is_target=true)")
	}
}

// ── Threads ──────────────────────────────────

func TestGetThreadMessages(t *testing.T) {
	list := callTool(t, "get_channel_messages", map[string]any{
		"channel": testRoomID, "count": 20, "group": false,
	})
	msgs := requireSlice(t, list, "messages")

	var threadID string
	for _, raw := range msgs {
		m := requireMap(t, raw)
		if tc, ok := m["thread_replies"].(float64); ok && tc > 0 {
			threadID = requireStr(t, m, "id")
			break
		}
	}
	if threadID == "" {
		t.Skip("no threads in test channel")
	}

	r := callTool(t, "get_thread_messages", map[string]any{"thread_id": threadID, "count": 10})
	replies := requireSlice(t, r, "messages")
	if len(replies) == 0 {
		t.Error("expected thread replies")
	}
}

// ── Users ──────────────────────────────────

func TestListUsers(t *testing.T) {
	r := callTool(t, "list_users", map[string]any{"count": 3})
	users := requireSlice(t, r, "users")
	if len(users) == 0 {
		t.Fatal("no users")
	}
	u := requireMap(t, users[0])
	requireStr(t, u, "id")
	requireStr(t, u, "username")
}

func TestGetUserInfo(t *testing.T) {
	list := callTool(t, "list_users", map[string]any{"count": 1})
	users := requireSlice(t, list, "users")
	username := requireStr(t, requireMap(t, users[0]), "username")

	r := callTool(t, "get_user_info", map[string]any{"username": username})
	requireStr(t, r, "id")
	requireStr(t, r, "username")
}

// ── Files ──────────────────────────────────

func TestListRoomFiles(t *testing.T) {
	r := callTool(t, "list_room_files", map[string]any{"channel": testRoomID, "count": 5})
	requireStr(t, r, "channel")
}

func TestDownloadImage(t *testing.T) {
	files := callTool(t, "list_room_files", map[string]any{"channel": testRoomID, "count": 20})
	fileList, _ := files["files"].([]any)

	var imageURL string
	for _, raw := range fileList {
		f := requireMap(t, raw)
		if ftype, _ := f["type"].(string); strings.HasPrefix(ftype, "image/") {
			imageURL, _ = f["url"].(string)
			break
		}
	}
	if imageURL == "" {
		t.Skip("no images in test channel")
	}

	content := callToolRaw(t, "download_file", map[string]any{"url": imageURL})
	var hasImage bool
	for _, c := range content {
		if c["type"] == "image" {
			hasImage = true
			if _, ok := c["data"].(string); !ok {
				t.Error("image should have base64 data")
			}
			if mime, _ := c["mimeType"].(string); !strings.HasPrefix(mime, "image/") {
				t.Errorf("expected image/*, got %s", mime)
			}
		}
	}
	if !hasImage {
		t.Error("expected image content type")
	}
}

// ── Digest, mentions, unread ──────────────────

func TestGetChannelDigest(t *testing.T) {
	r := callTool(t, "get_channel_digest", map[string]any{"channel": testRoomID, "hours": 168})
	requireStr(t, r, "channel")
	if _, ok := r["total_messages"].(float64); !ok {
		t.Error("total_messages should be number")
	}
	// active_users/threads/files_shared may be null when channel is empty
	for _, key := range []string{"active_users", "threads", "files_shared"} {
		v := r[key]
		if v != nil {
			if _, ok := v.([]any); !ok {
				t.Errorf("%s should be array or null, got %T", key, v)
			}
		}
	}
}

func TestGetMentions(t *testing.T) {
	r := callTool(t, "get_mentions", map[string]any{"channel": testRoomID, "count": 5})
	requireStr(t, r, "channel")
	if _, ok := r["messages"].([]any); !ok {
		t.Fatal("messages should be array")
	}
}

func TestGetUnreadChannels(t *testing.T) {
	r := callTool(t, "get_unread_channels", map[string]any{})
	if _, ok := r["total"].(float64); !ok {
		t.Error("total should be number")
	}
}

func TestGetPinnedMessages(t *testing.T) {
	r := callTool(t, "get_pinned_messages", map[string]any{"channel": testRoomID, "count": 5})
	requireStr(t, r, "channel")
	if _, ok := r["messages"].([]any); !ok {
		t.Error("messages should be array")
	}
}

// ── Write: send -> edit -> pin -> unpin -> delete ──

func TestSendEditPinDeleteMessage(t *testing.T) {
	ts := time.Now().Format("15:04:05")

	// Send
	sent := callTool(t, "send_message", map[string]any{
		"channel": testRoomID, "text": "[test] " + ts,
	})
	if requireStr(t, sent, "status") != "sent" {
		t.Fatal("expected status=sent")
	}
	msgID := requireStr(t, sent, "id")

	// Edit
	edited := callTool(t, "edit_message", map[string]any{
		"room_id": testRoomID, "message_id": msgID, "text": "[test] edited " + ts,
	})
	if requireStr(t, edited, "status") != "edited" {
		t.Error("expected status=edited")
	}

	// Pin
	pinned := callTool(t, "pin_message", map[string]any{"message_id": msgID})
	if requireStr(t, pinned, "status") != "pinned" {
		t.Error("expected status=pinned")
	}

	// Unpin
	unpinned := callTool(t, "unpin_message", map[string]any{"message_id": msgID})
	if requireStr(t, unpinned, "status") != "unpinned" {
		t.Error("expected status=unpinned")
	}

	// Delete (cleanup)
	deleted := callTool(t, "delete_message", map[string]any{
		"room_id": testRoomID, "message_id": msgID,
	})
	if requireStr(t, deleted, "status") != "deleted" {
		t.Error("expected status=deleted")
	}
}

func TestReplyToThread(t *testing.T) {
	ts := time.Now().Format("15:04:05")

	// Send parent
	parent := callTool(t, "send_message", map[string]any{
		"channel": testRoomID, "text": "[test] thread " + ts,
	})
	parentID := requireStr(t, parent, "id")

	// Reply
	reply := callTool(t, "reply_to_thread", map[string]any{
		"thread_id": parentID, "text": "[test] reply " + ts,
	})
	if requireStr(t, reply, "status") != "sent" {
		t.Error("expected status=sent")
	}
	replyID := requireStr(t, reply, "id")

	// Verify thread
	thread := callTool(t, "get_thread_messages", map[string]any{"thread_id": parentID})
	replies := requireSlice(t, thread, "messages")
	if len(replies) == 0 {
		t.Error("expected thread replies")
	}

	// Cleanup
	callTool(t, "delete_message", map[string]any{"room_id": testRoomID, "message_id": replyID})
	callTool(t, "delete_message", map[string]any{"room_id": testRoomID, "message_id": parentID})
}

func TestAddReaction(t *testing.T) {
	ts := time.Now().Format("15:04:05")

	sent := callTool(t, "send_message", map[string]any{
		"channel": testRoomID, "text": "[test] reaction " + ts,
	})
	msgID := requireStr(t, sent, "id")

	reacted := callTool(t, "add_reaction", map[string]any{
		"message_id": msgID, "emoji": "thumbsup",
	})
	if requireStr(t, reacted, "status") != "reacted" {
		t.Error("expected status=reacted")
	}

	// Cleanup
	callTool(t, "delete_message", map[string]any{"room_id": testRoomID, "message_id": msgID})
}

// ── Attachment URLs ──────────────────────────

func TestAttachmentURLsAbsolute(t *testing.T) {
	r := callTool(t, "get_channel_messages", map[string]any{
		"channel": testRoomID, "count": 50, "group": false,
	})
	msgs := requireSlice(t, r, "messages")

	for _, raw := range msgs {
		m := requireMap(t, raw)
		if f, ok := m["file"].(map[string]any); ok {
			furl, _ := f["url"].(string)
			if furl != "" && !strings.HasPrefix(furl, "http") {
				t.Errorf("file url not absolute: %s", furl)
			}
		}
		if atts, ok := m["attachments"].([]any); ok {
			for _, araw := range atts {
				a := requireMap(t, araw)
				for _, key := range []string{"url", "image_url"} {
					v, _ := a[key].(string)
					if v != "" && !strings.HasPrefix(v, "http") {
						t.Errorf("attachment %s not absolute: %s", key, v)
					}
				}
			}
		}
	}
}

// ── Read-only mode ──────────────────────────

func TestReadOnlyBlocksAllWrites(t *testing.T) {
	readOnly = true
	defer func() { readOnly = false }()

	writeTools := []string{
		"send_message", "send_dm", "reply_to_thread", "add_reaction",
		"send_file", "edit_message", "delete_message", "pin_message", "unpin_message",
	}
	for _, tool := range writeTools {
		rpc, err := callToolOnce(tool, map[string]any{})
		if err != nil {
			t.Fatalf("decode %s: %v", tool, err)
		}
		if !rpc.Result.IsError {
			t.Errorf("%s should be blocked in read-only mode", tool)
			continue
		}
		errText := rpc.Result.Content[0].Text
		if !strings.Contains(errText, "read-only") {
			t.Errorf("%s error should mention read-only, got: %s", tool, errText)
		}
	}
}
