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
	waitFor(t, "lead and sub each called once", func() bool { return len(leadIn.snapshot()) == 1 && len(subIn.snapshot()) == 1 })
	time.Sleep(100 * time.Millisecond) // nothing else arrives

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
	if len(leadIn.snapshot()) != 1 || len(subIn.snapshot()) != 1 {
		t.Fatalf("unaddressed chat reached a role: lead %d, sub %d", len(leadIn.snapshot()), len(subIn.snapshot()))
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

	sendAs(t, sub, rc, "开始跑了，先编译", false)
	time.Sleep(100 * time.Millisecond)
	if n := len(leadIn.snapshot()); n != 0 {
		t.Fatalf("interim output woke the lead (%d messages)", n)
	}
	sendAs(t, sub, rc, "测试全过（第一段）", true)
	sendAs(t, sub, rc, "（第二段）", true)
	waitFor(t, "both answer chunks at the lead", func() bool { return len(leadIn.snapshot()) == 2 })
	time.Sleep(100 * time.Millisecond)

	// The first chunk @-s the lead and starts its check; the second reaches it as a teammate's
	// context only (core never starts a turn from FromBot without Mentioned).
	back := leadIn.snapshot()
	if len(back) != 2 || !back[0].FromBot || !back[0].Mentioned || !strings.Contains(back[0].Content, "测试全过") {
		t.Fatalf("lead got %+v, want the first answer chunk as a teammate's @", back)
	}
	if !back[1].FromBot || back[1].Mentioned {
		t.Fatalf("second chunk = %+v, want teammate context, not a call", back[1])
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
			f, lead, _, leadIn, subIn := startTeam(t, allow)
			f.say(teamGroup, strangeQQ, "路人", at(leadQQ), text(" rm -rf 一下"))
			sendAs(t, lead, groupCtx(), "@龙娘 帮个忙", true)
			waitFor(t, "teammate hand-off", func() bool { return len(subIn.snapshot()) == 1 })
			time.Sleep(100 * time.Millisecond)
			if n := len(leadIn.snapshot()); n != 0 {
				t.Fatalf("a stranger reached the lead with allow_from=%q", allow)
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
	time.Sleep(100 * time.Millisecond)
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
	waitFor(t, "group list loaded", func() bool { return lead.knownGroup("500") })
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
