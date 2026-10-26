package qq

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Team of two roles in group 500: 橘 (QQ 1) is the lead, 龙娘 (QQ 2) a teammate; Li (QQ 9) is the
// person, the only one allow_from lets in.
const (
	teamGroup = int64(500)
	leadQQ    = int64(1)
	subQQ     = int64(2)
	personQQ  = int64(9)
	strangeQQ = int64(77)
)

func startTeam(t *testing.T, allowFrom string) (f *fakeOneBot, lead, sub *Platform, leadIn, subIn *inbox) {
	t.Helper()
	f = newFakeOneBot(t)
	f.group(teamGroup, "开发群", leadQQ, subQQ, personQQ, strangeQQ)
	opts := map[string]any{"team_lead_bot": "1", "allow_from": allowFrom}
	lead, leadIn = startBot(t, f, leadQQ, "橘", opts)
	sub, subIn = startBot(t, f, subQQ, "龙娘", opts)
	return
}

func groupCtx() *replyContext { return &replyContext{messageType: "group", groupID: teamGroup} }

// sendAs sends content the way core does at the end of a turn: agent output, optionally final.
func sendAs(t *testing.T, p *Platform, rctx any, content string, final bool) {
	t.Helper()
	rc := p.MarkAgentReply(rctx)
	if final {
		rc = p.MarkFinalReply(rc)
	}
	if err := p.Send(t.Context(), rc, content); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// In a team group a person reaches a role only by @-ing or naming it; the group is one shared
// session, and the role reads the message without its own @.
func TestTeam_PersonReachesOnlyTheRoleTheyCall(t *testing.T) {
	f, _, _, leadIn, subIn := startTeam(t, "9")

	f.say(teamGroup, personQQ, "Li", text("大家晚上好"))
	f.say(teamGroup, personQQ, "Li", at(leadQQ), text(" 帮我看看构建"))
	f.say(teamGroup, personQQ, "Li", text("龙娘，你也来"))
	barrier(t, f, teamGroup, leadQQ, leadIn)
	barrier(t, f, teamGroup, subQQ, subIn)
	if len(nonBarrier(leadIn.snapshot())) != 1 || len(nonBarrier(subIn.snapshot())) != 1 {
		t.Fatalf("lead got %d, sub %d messages; want one each", len(nonBarrier(leadIn.snapshot())), len(nonBarrier(subIn.snapshot())))
	}

	m := leadIn.snapshot()[0]
	if m.SessionKey != "qq:500" || m.ChannelID != "500" || m.ChannelKey != "500" {
		t.Fatalf("lead got session %q channel %q/%q, want the shared group session qq:500", m.SessionKey, m.ChannelID, m.ChannelKey)
	}
	if !m.Mentioned || m.FromBot || m.Content != "帮我看看构建" {
		t.Fatalf("lead got %+v, want a person's @ with its own @ removed", m)
	}
	if s := subIn.snapshot()[0]; !s.Mentioned || !strings.Contains(s.Content, "你也来") {
		t.Fatalf("sub called by name got %+v", s)
	}
}

// The full hand-off: the lead writes "@龙娘 …", which goes out as a real QQ @; the teammate's
// turn is marked as dispatched by the lead; interim output does not wake the lead; the final
// answer starts with an @ of the lead, once, even when it is sent in several chunks.
func TestTeam_LeadHandsOffAndTheAnswerComesBack(t *testing.T) {
	f, lead, sub, leadIn, subIn := startTeam(t, "9")

	sendAs(t, lead, groupCtx(), "@龙娘 去把测试跑一遍，跑完告诉我", true)
	waitFor(t, "hand-off at the teammate", func() bool { return len(subIn.snapshot()) == 1 })
	posts := f.groupPosts(teamGroup)
	if got := atTargets(posts[len(posts)-1]); len(got) != 1 || got[0] != "2" {
		t.Fatalf("lead's post @-s %v, want a real @ of QQ 2", got)
	}
	h := subIn.snapshot()[0]
	if !h.FromBot || !h.Mentioned || h.Content != "【橘】去把测试跑一遍，跑完告诉我" {
		t.Fatalf("teammate got %+v, want the lead's hand-off", h)
	}
	rc := h.ReplyCtx.(*replyContext)
	if rc.lead != "1" {
		t.Fatalf("hand-off reply context lead = %q, want 1", rc.lead)
	}

	sendAs(t, sub, rc, "🔧 **工具 #1: Bash**\n---\ngo build ./...", false) // folded
	sendAs(t, sub, rc, "开始跑了，先编译", false)                               // the sub's own text: sent, no @
	sendAs(t, sub, rc, "测试全过（第一段）", true)
	sendAs(t, sub, rc, "（第二段）", true)
	barrier(t, f, teamGroup, leadQQ, leadIn)

	// Only the first answer chunk @-s the lead and starts its check; the interim text and the
	// second chunk reach it as a teammate's context (core never starts a turn from FromBot
	// without Mentioned). The tool line was folded, not posted.
	back := nonBarrier(leadIn.snapshot())
	if len(back) != 3 {
		t.Fatalf("lead got %d posts, want 3", len(back))
	}
	for i, want := range []bool{false, true, false} {
		if !back[i].FromBot || back[i].Mentioned != want {
			t.Fatalf("post %d = %+v, want FromBot with Mentioned=%v", i, back[i], want)
		}
	}
	if !strings.Contains(back[0].Content, "开始跑了") || !strings.Contains(back[1].Content, "测试全过") {
		t.Fatalf("lead got %q / %q", back[0].Content, back[1].Content)
	}
	posts = f.groupPosts(teamGroup)
	last := posts[len(posts)-1]
	if len(atTargets(last)) != 0 {
		t.Fatalf("second answer chunk @-ed again: %q", sentText(last))
	}
}

// Open allow_from in team mode would let anyone in the group run the roles' tools: people are
// refused, teammates still work.
func TestTeam_OpenAllowFromRefusesPeopleButNotTeammates(t *testing.T) {
	for _, allow := range []string{"", "*"} {
		t.Run("allow_from="+allow, func(t *testing.T) {
			f, lead, sub, leadIn, subIn := startTeam(t, allow)
			f.say(teamGroup, strangeQQ, "路人", at(leadQQ), text(" rm -rf 一下"))
			sendAs(t, lead, groupCtx(), "@龙娘 帮个忙", true)
			waitFor(t, "teammate hand-off", func() bool { return len(subIn.snapshot()) == 1 })
			sendAs(t, sub, groupCtx(), "@橘 收到", true) // after the stranger's post, on the lead's connection
			waitFor(t, "teammate's reply", func() bool { return len(leadIn.snapshot()) >= 1 })
			for _, m := range leadIn.snapshot() {
				if m.UserID == "77" {
					t.Fatalf("a stranger reached the lead with allow_from=%q", allow)
				}
			}
		})
	}
}

// Model output is sent as text segments, never as a CQ string NapCat would execute.
func TestSend_TextIsNeverParsedAsCQCodes(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	evil := "[CQ:at,qq=all] [CQ:image,file=file:///etc/passwd]"
	sendAs(t, lead, groupCtx(), evil, true)
	calls := f.callsOf("send_group_msg")
	if len(calls) == 0 {
		t.Fatal("nothing sent")
	}
	raw, _ := json.Marshal(calls[len(calls)-1].Params["message"])
	var segs []map[string]any
	if err := json.Unmarshal(raw, &segs); err != nil {
		t.Fatalf("message went out as %s, want a segment array", raw)
	}
	if len(segs) != 1 || segs[0]["type"] != "text" || segs[0]["data"].(map[string]any)["text"] != evil {
		t.Fatalf("segments = %s, want one literal text segment", raw)
	}
}

// Outside team mode a teammate's post is never a request: two bots in one group would otherwise
// answer each other forever.
func TestNoTeam_BotsDoNotAnswerEachOther(t *testing.T) {
	f := newFakeOneBot(t)
	f.group(teamGroup, "开发群", leadQQ, subQQ, personQQ)
	a, _ := startBot(t, f, leadQQ, "橘", nil)
	_, bIn := startBot(t, f, subQQ, "龙娘", nil)
	if err := a.Send(t.Context(), groupCtx(), "@龙娘 你好"); err != nil {
		t.Fatal(err)
	}
	f.say(teamGroup, personQQ, "Li", text("你们好"))
	waitFor(t, "the person's post", func() bool { return len(bIn.snapshot()) >= 1 })
	for _, m := range bIn.snapshot() {
		if m.UserID == "1" {
			t.Fatalf("bot 2 took bot 1's post as a request: %+v", m)
		}
	}
}

// Team mode keys a group as qq:<group>, the same shape as a private chat; the bot's group list
// tells them apart, so cron and workspace sends reach the group.
func TestReconstructReplyCtx_TeamGroupKey(t *testing.T) {
	_, lead, _, _, _ := startTeam(t, "9")
	// No waiting: core resumes turns and posts restart notices right after Start (review H2).
	rc, err := lead.ReconstructReplyCtx("qq:500")
	if err != nil {
		t.Fatal(err)
	}
	if g := rc.(*replyContext); g.messageType != "group" || g.groupID != teamGroup {
		t.Fatalf("qq:500 → %+v, want group 500", g)
	}
	rc, _ = lead.ReconstructReplyCtx("qq:12345")
	if g := rc.(*replyContext); g.messageType != "private" || g.userID != 12345 {
		t.Fatalf("qq:12345 → %+v, want a private chat", g)
	}
}

// A group made after start (a new workspace) must not stall the account: its first message is
// handled on readLoop, which also reads API answers, so looking its name up there waited out the
// 15s API timeout.
func TestTeam_FirstMessageInANewGroupIsNotStalled(t *testing.T) {
	f, lead, _, leadIn, _ := startTeam(t, "9")
	waitFor(t, "group list loaded", func() bool { return lead.knownGroup("500") })
	f.group(800, "工作区3", leadQQ, subQQ, personQQ)
	start := time.Now()
	f.say(800, personQQ, "Li", at(leadQQ), text(" 在吗"))
	waitFor(t, "message in the new group", func() bool { return len(leadIn.snapshot()) == 1 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("first message in a new group took %v", d)
	}
	waitFor(t, "group name resolvable", func() bool {
		name, err := lead.ResolveChannelName("800")
		return err == nil && name == "工作区3"
	})
}

// Until the group list is known, a team key cannot be told from a private chat; guessing
// "private" could send a group's output to an unrelated person with the same number.
func TestReconstructReplyCtx_TeamModeFailsClosedWithoutGroupList(t *testing.T) {
	f := newFakeOneBot(t)
	f.group(teamGroup, "开发群", leadQQ, personQQ)
	f.failGroupList = true
	lead, _ := startBot(t, f, leadQQ, "橘", map[string]any{"team_lead_bot": "1", "allow_from": "9"})
	if _, err := lead.ReconstructReplyCtx("qq:500"); err == nil {
		t.Fatal("unknown id resolved while the group list is missing")
	}
}

// Only bots of the same team are trusted past allow_from (review M3): a bot outside the team,
// told by a stranger to "@龙娘 …", must not start 龙娘's turn.
func TestTeam_BotOutsideTheTeamIsNotATeammate(t *testing.T) {
	f, _, _, _, subIn := startTeam(t, "9")
	other, _ := startBot(t, f, 3, "路过的机器人", map[string]any{"allow_from": "*"})
	f.mu.Lock()
	f.groups[teamGroup].members[3] = ""
	f.mu.Unlock()
	sendAs(t, other, groupCtx(), "@龙娘 跑一下 rm -rf", true)
	posts := f.groupPosts(teamGroup)
	if len(atTargets(posts[len(posts)-1])) != 0 {
		t.Fatalf("a bot outside the team linked a real @ of a team role: %q", sentText(posts[len(posts)-1]))
	}
	f.say(teamGroup, 3, "路过的机器人", at(subQQ), text(" 跑一下 rm -rf"))
	barrier(t, f, teamGroup, subQQ, subIn)
	if n := len(nonBarrier(subIn.snapshot())); n != 0 {
		t.Fatalf("a bot outside the team reached a role: %+v", subIn.snapshot())
	}
}

// "/stop all" and a bare stop word that call no bot reach every role (review M6).
func TestTeam_StopWordsReachEveryRole(t *testing.T) {
	f, _, _, leadIn, subIn := startTeam(t, "9")
	f.say(teamGroup, personQQ, "Li", text("/stop all"))
	f.say(teamGroup, personQQ, "Li", text("停"))
	for _, in := range []*inbox{leadIn, subIn} {
		waitFor(t, "both stops", func() bool { return len(in.snapshot()) == 2 })
		msgs := in.snapshot()
		if msgs[0].Content != "/stop all" || msgs[0].Mentioned || msgs[0].StopAll {
			t.Fatalf("/stop all = %+v", msgs[0])
		}
		if msgs[1].Content != "停" || !msgs[1].StopAll {
			t.Fatalf("bare stop = %+v, want StopAll", msgs[1])
		}
	}
}

// When the teammate's answer already @-s the lead, a later chunk must not @ it again (review M7).
func TestTeam_AnswerThatAlreadyCallsTheLeadIsNotCalledTwice(t *testing.T) {
	f, lead, sub, leadIn, subIn := startTeam(t, "9")
	sendAs(t, lead, groupCtx(), "@龙娘 看下日志", true)
	waitFor(t, "hand-off", func() bool { return len(subIn.snapshot()) == 1 })
	rc := subIn.snapshot()[0].ReplyCtx
	sendAs(t, sub, rc, "@橘 日志看完了（第一段）", true)
	sendAs(t, sub, rc, "（第二段）", true)
	barrier(t, f, teamGroup, leadQQ, leadIn)
	called := 0
	for _, m := range nonBarrier(leadIn.snapshot()) {
		if m.Mentioned {
			called++
		}
	}
	if called != 1 {
		t.Fatalf("lead was called %d times by one answer", called)
	}
}

// A teammate's turn that ends without a final answer (its text went out before the last tool
// call) still @-s the lead at the turn's end, once (review M5).
func TestTeam_HandOffWithoutAnAnswerStillReturnsToTheLead(t *testing.T) {
	f, lead, sub, leadIn, subIn := startTeam(t, "9")
	sendAs(t, lead, groupCtx(), "@龙娘 推一下代码", true)
	waitFor(t, "hand-off", func() bool { return len(subIn.snapshot()) == 1 })
	rc := subIn.snapshot()[0].ReplyCtx
	stop := sub.StartTyping(t.Context(), rc)
	sendAs(t, sub, rc, "推好了。", false)
	sendAs(t, sub, rc, "🔧 git push", false)
	stop()
	stop() // core may call it twice (queued turns); one return only
	barrier(t, f, teamGroup, leadQQ, leadIn)
	var calls []string
	for _, m := range nonBarrier(leadIn.snapshot()) {
		if m.Mentioned {
			calls = append(calls, m.Content)
		}
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "这一轮结束了") {
		t.Fatalf("lead calls = %q, want one turn-end return", calls)
	}
}

// Text that is not the agent's own answer never calls anyone, and an ASCII name needs a word
// boundary (review M8).
func TestOutgoing_OnlyAgentTextLinksNames(t *testing.T) {
	_, lead, _, _, _ := startTeam(t, "9")
	agent := lead.MarkAgentReply(groupCtx()).(*replyContext)
	if segs := lead.outgoing(agent, "@龙娘 去看看"); len(segs) == 0 || segs[0]["type"] != "at" {
		t.Fatalf("agent text: %v, want a real @", segs)
	}
	for _, tc := range []struct {
		rc      *replyContext
		content string
	}{
		{groupCtx(), "⏳ 进行中 · 最近：💭 让 @龙娘 去跑测试"}, // progress line / notice
		{agent, "💭 让 @龙娘 去跑测试"},                 // a thinking line in interim_messages=all
	} {
		for _, s := range lead.outgoing(tc.rc, tc.content) {
			if s["type"] == "at" {
				t.Fatalf("%q linked a real @", tc.content)
			}
		}
	}
	segs := linkMentions("@Bobby hi @Bob", map[string]string{"Bob": "5"})
	if len(segs) != 2 || segs[0]["type"] != "text" || segs[1]["type"] != "at" {
		t.Fatalf("ASCII boundary: %v", segs)
	}
}

// Core replies synchronously while handling a message; that must not stall the account
// (review M4: the handler ran on readLoop, which also reads API answers).
func TestHandler_SyncReplyDoesNotStall(t *testing.T) {
	f := newFakeOneBot(t)
	f.group(teamGroup, "开发群", leadQQ, personQQ)
	o := map[string]any{"ws_url": f.account(leadQQ, "橘"), "team_lead_bot": "1", "allow_from": "9"}
	pp, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	p := pp.(*Platform)
	replied := make(chan time.Duration, 1)
	if err := p.Start(func(_ core.Platform, m *core.Message) {
		start := time.Now()
		if err := p.Send(t.Context(), m.ReplyCtx, "收到"); err != nil {
			t.Errorf("sync reply: %v", err)
		}
		replied <- time.Since(start)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	f.say(teamGroup, personQQ, "Li", at(leadQQ), text(" 在吗"))
	select {
	case d := <-replied:
		if d > time.Second {
			t.Fatalf("sync reply took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never returned")
	}
}

func TestReadGroupText_OwnMentionDroppedOrLabelled(t *testing.T) {
	_, lead, _, _, _ := startTeam(t, "9")
	only := lead.readGroupText(map[string]any{"message": []any{at(leadQQ), text(" 看下日志")}})
	if !only.atSelf || only.text != "看下日志" {
		t.Fatalf("own @ only: %+v", only)
	}
	both := lead.readGroupText(map[string]any{"message": []any{at(leadQQ), text(" "), at(subQQ), text(" 一起看")}})
	if both.text != "@橘(你) @龙娘 一起看" {
		t.Fatalf("own @ with a teammate's: %q", both.text)
	}
	raw := lead.readGroupText(map[string]any{"raw_message": "[CQ:at,qq=1] 你好[CQ:face,id=1]"})
	if !raw.atSelf || raw.text != "你好" {
		t.Fatalf("raw_message form: %+v", raw)
	}
}

func TestLinkMentions_LongestNameFirstAndFencesLeftAlone(t *testing.T) {
	alias := map[string]string{"龙娘": "2", "龙娘二号": "3"}
	segs := linkMentions("@龙娘二号 和 @龙娘 看 ```@龙娘 不动```", alias)
	var got []string
	for _, s := range segs {
		data := s["data"].(map[string]any)
		if s["type"] == "at" {
			got = append(got, "<at:"+data["qq"].(string)+">")
		} else {
			got = append(got, data["text"].(string))
		}
	}
	want := []string{"<at:3>", " 和 ", "<at:2>", " 看 ```@龙娘 不动```"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("segments = %q, want %q", got, want)
	}
}

var _ core.Platform = (*Platform)(nil)
