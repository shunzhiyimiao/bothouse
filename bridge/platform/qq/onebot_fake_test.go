package qq

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

// fakeOneBot is a OneBot v11 server holding several QQ accounts, the way one NapCat per account
// would look to cc-connect (issue #344, step 2). Each account connects on its own path
// (/acc/<qq>). What one account posts to a group is delivered to every other member account as a
// message event, as real QQ does, so a team of bots in one process can be driven end to end.
type fakeOneBot struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	accounts map[int64]*fakeAccount
	groups   map[int64]*fakeGroup
	calls    []fakeCall
	nextID   int64
	sent     map[int64]fakeSent // message_id → what was posted

	failGroupList bool // get_group_list answers an error
}

type fakeAccount struct {
	id   int64
	nick string
	wmu  sync.Mutex
	conn *websocket.Conn
}

type fakeGroup struct {
	id      int64
	name    string
	members map[int64]string // QQ → group card ("" = use nickname)
}

type fakeCall struct {
	Self   int64
	Action string
	Params map[string]any
}

type fakeSent struct {
	Self    int64
	GroupID int64
	UserID  int64
	Message []map[string]any
	Deleted bool
}

func newFakeOneBot(t *testing.T) *fakeOneBot {
	t.Helper()
	f := &fakeOneBot{t: t, accounts: map[int64]*fakeAccount{}, groups: map[int64]*fakeGroup{}, nextID: 1000, sent: map[int64]fakeSent{}}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/acc/"), 10, 64)
		f.mu.Lock()
		acc := f.accounts[id]
		f.mu.Unlock()
		if err != nil || acc == nil {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		acc.wmu.Lock()
		acc.conn = c
		acc.wmu.Unlock()
		f.serve(acc, c)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// account registers a QQ account; its platform connects to the returned ws URL.
func (f *fakeOneBot) account(id int64, nick string) string {
	f.mu.Lock()
	f.accounts[id] = &fakeAccount{id: id, nick: nick}
	f.mu.Unlock()
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/acc/" + strconv.FormatInt(id, 10)
}

// group creates a group with the given member QQ numbers.
func (f *fakeOneBot) group(id int64, name string, members ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := &fakeGroup{id: id, name: name, members: map[int64]string{}}
	for _, m := range members {
		g.members[m] = ""
	}
	f.groups[id] = g
}

func (f *fakeOneBot) serve(acc *fakeAccount, c *websocket.Conn) {
	defer c.Close()
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			Action string         `json:"action"`
			Echo   string         `json:"echo"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{Self: acc.id, Action: req.Action, Params: req.Params})
		f.mu.Unlock()
		data, retcode := f.handle(acc, req.Action, req.Params)
		status := "ok"
		if retcode != 0 {
			status = "failed"
		}
		f.write(acc, map[string]any{"status": status, "retcode": retcode, "echo": req.Echo, "data": data})
	}
}

func (f *fakeOneBot) write(acc *fakeAccount, v any) {
	raw, _ := json.Marshal(v)
	acc.wmu.Lock()
	defer acc.wmu.Unlock()
	if acc.conn != nil {
		_ = acc.conn.WriteMessage(websocket.TextMessage, raw)
	}
}

func (f *fakeOneBot) handle(acc *fakeAccount, action string, params map[string]any) (any, int) {
	switch action {
	case "get_login_info":
		return map[string]any{"user_id": acc.id, "nickname": acc.nick}, 0
	case "get_group_info":
		f.mu.Lock()
		defer f.mu.Unlock()
		if g := f.groups[int64(num(params["group_id"]))]; g != nil {
			return map[string]any{"group_id": g.id, "group_name": g.name, "member_count": len(g.members)}, 0
		}
		return nil, 1404
	case "get_group_list":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failGroupList {
			return nil, 1404
		}
		list := []map[string]any{}
		for _, g := range f.groups {
			if _, in := g.members[acc.id]; in {
				list = append(list, map[string]any{"group_id": g.id, "group_name": g.name})
			}
		}
		return list, 0
	case "get_group_member_list":
		f.mu.Lock()
		defer f.mu.Unlock()
		g := f.groups[int64(num(params["group_id"]))]
		if g == nil {
			return nil, 1404
		}
		list := []map[string]any{}
		for id, card := range g.members {
			nick := strconv.FormatInt(id, 10)
			if a := f.accounts[id]; a != nil {
				nick = a.nick
			}
			list = append(list, map[string]any{"user_id": id, "nickname": nick, "card": card})
		}
		return list, 0
	case "send_group_msg":
		gid := int64(num(params["group_id"]))
		segs := segmentsOf(params["message"])
		id := f.record(fakeSent{Self: acc.id, GroupID: gid, Message: segs})
		f.deliver(gid, acc.id, acc.nick, id, segs)
		return map[string]any{"message_id": id}, 0
	case "send_private_msg":
		id := f.record(fakeSent{Self: acc.id, UserID: int64(num(params["user_id"])), Message: segmentsOf(params["message"])})
		return map[string]any{"message_id": id}, 0
	case "delete_msg":
		f.mu.Lock()
		defer f.mu.Unlock()
		id := int64(num(params["message_id"]))
		s, ok := f.sent[id]
		if !ok || s.Self != acc.id {
			return nil, 1404
		}
		s.Deleted = true
		f.sent[id] = s
		return nil, 0
	}
	return nil, 1404
}

func (f *fakeOneBot) record(s fakeSent) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.sent[f.nextID] = s
	return f.nextID
}

// deliver hands a group message to every member account except the sender.
func (f *fakeOneBot) deliver(gid, from int64, nick string, id int64, segs []map[string]any) {
	f.mu.Lock()
	g := f.groups[gid]
	var to []*fakeAccount
	if g != nil {
		for m := range g.members {
			if a := f.accounts[m]; a != nil && m != from {
				to = append(to, a)
			}
		}
	}
	f.mu.Unlock()
	if g == nil {
		f.t.Errorf("fake onebot: message to unknown group %d", gid)
		return
	}
	for _, a := range to {
		f.write(a, map[string]any{
			"post_type": "message", "message_type": "group", "sub_type": "normal",
			"time": time.Now().Unix(), "self_id": a.id,
			"user_id": from, "group_id": gid, "message_id": id,
			"message": segs, "raw_message": rawOf(segs),
			"sender": map[string]any{"user_id": from, "nickname": nick},
		})
	}
}

// say posts a message from a person (not one of the fake's accounts) into a group.
func (f *fakeOneBot) say(gid, user int64, nick string, segs ...map[string]any) int64 {
	id := f.record(fakeSent{Self: user, GroupID: gid, Message: segs})
	f.deliver(gid, user, nick, id, segs)
	return id
}

// groupPosts returns what accounts posted to a group, oldest first.
func (f *fakeOneBot) groupPosts(gid int64) []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeSent
	for id := int64(1001); id <= f.nextID; id++ {
		if s, ok := f.sent[id]; ok && s.GroupID == gid && f.accounts[s.Self] != nil {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeOneBot) callsOf(action string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Action == action {
			out = append(out, c)
		}
	}
	return out
}

func text(s string) map[string]any {
	return map[string]any{"type": "text", "data": map[string]any{"text": s}}
}

func at(qq int64) map[string]any {
	return map[string]any{"type": "at", "data": map[string]any{"qq": strconv.FormatInt(qq, 10)}}
}

// segmentsOf normalises an outgoing message the way the fake reads it: a segment array is kept,
// a string becomes one text segment (the fake never parses CQ codes; see sentText).
func segmentsOf(v any) []map[string]any {
	switch m := v.(type) {
	case string:
		return []map[string]any{text(m)}
	case []any:
		var out []map[string]any
		for _, s := range m {
			if seg, ok := s.(map[string]any); ok {
				out = append(out, seg)
			}
		}
		return out
	}
	return nil
}

// rawOf renders segments as OneBot raw_message (CQ codes for non-text segments).
func rawOf(segs []map[string]any) string {
	var b strings.Builder
	for _, s := range segs {
		data, _ := s["data"].(map[string]any)
		switch s["type"] {
		case "text":
			t, _ := data["text"].(string)
			b.WriteString(t)
		case "at":
			fmt.Fprintf(&b, "[CQ:at,qq=%v]", data["qq"])
		default:
			fmt.Fprintf(&b, "[CQ:%v]", s["type"])
		}
	}
	return b.String()
}

// sentText is a post as people read it: text plus "@<qq>" for each at segment.
func sentText(s fakeSent) string {
	var b strings.Builder
	for _, seg := range s.Message {
		data, _ := seg["data"].(map[string]any)
		switch seg["type"] {
		case "text":
			t, _ := data["text"].(string)
			b.WriteString(t)
		case "at":
			fmt.Fprintf(&b, "@%v", data["qq"])
		}
	}
	return b.String()
}

// atTargets lists the QQ numbers a post @-s with real at segments.
func atTargets(s fakeSent) []string {
	var out []string
	for _, seg := range s.Message {
		if seg["type"] == "at" {
			data, _ := seg["data"].(map[string]any)
			out = append(out, fmt.Sprint(data["qq"]))
		}
	}
	return out
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// inbox collects the messages a platform hands to core.
type inbox struct {
	mu   sync.Mutex
	msgs []*core.Message
}

func (in *inbox) handler(_ core.Platform, m *core.Message) {
	in.mu.Lock()
	in.msgs = append(in.msgs, m)
	in.mu.Unlock()
}

func (in *inbox) snapshot() []*core.Message {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]*core.Message(nil), in.msgs...)
}

// barrier makes "nothing else arrived" checks exact: a person @-s bot with a marker and the test
// waits for it. Events reach a bot in order on one connection and are handled in order, so
// everything posted before the barrier has been handled once it arrives.
func barrier(t *testing.T, f *fakeOneBot, gid, bot int64, in *inbox) {
	t.Helper()
	id := strconv.FormatInt(f.say(gid, 9, "Li", at(bot), text(" "+barrierText)), 10)
	waitFor(t, "barrier", func() bool {
		for _, m := range in.snapshot() {
			if m.MessageID == id {
				return true
			}
		}
		return false
	})
}

const barrierText = "（屏障）"

// nonBarrier drops barrier messages.
func nonBarrier(msgs []*core.Message) []*core.Message {
	var out []*core.Message
	for _, m := range msgs {
		if !strings.Contains(m.Content, barrierText) {
			out = append(out, m)
		}
	}
	return out
}

// waitFor polls cond for up to 3s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startBot creates and starts a qq platform on the fake for account id, stopped at test end.
func startBot(t *testing.T, f *fakeOneBot, id int64, nick string, opts map[string]any) (*Platform, *inbox) {
	t.Helper()
	o := map[string]any{"ws_url": f.account(id, nick)}
	for k, v := range opts {
		o[k] = v
	}
	p, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := &inbox{}
	qp := p.(*Platform)
	if err := qp.Start(in.handler); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = qp.Stop() })
	waitFor(t, "login of "+nick, func() bool { return qp.selfIDValue() == id })
	return qp, in
}

// The fake itself: two accounts in one group see each other's posts, a person's post reaches
// both, and the API answers what the platform asks at start.
func TestFakeOneBot_DeliversGroupPostsToOtherMembers(t *testing.T) {
	f := newFakeOneBot(t)
	f.group(500, "开发群", 1, 2, 9)
	team := map[string]any{"team_lead_bot": "1", "allow_from": "9"}
	a, aIn := startBot(t, f, 1, "橘", team)
	_, bIn := startBot(t, f, 2, "龙娘", team)

	f.say(500, 9, "Li", at(1), text(" "), at(2), text(" 大家好"))
	waitFor(t, "person's post at both bots", func() bool { return len(aIn.snapshot()) == 1 && len(bIn.snapshot()) == 1 })

	rc := &replyContext{messageType: "group", groupID: 500}
	if err := a.Send(t.Context(), rc, "收到"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "bot post at the other bot", func() bool { return len(bIn.snapshot()) == 2 })
	if got := len(aIn.snapshot()); got != 1 {
		t.Fatalf("sender saw its own post: %d messages", got)
	}
	if len(f.callsOf("get_login_info")) != 2 {
		t.Fatalf("get_login_info calls = %d, want one per bot", len(f.callsOf("get_login_info")))
	}
}
