package qq

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Team mode for QQ over OneBot (issue #344): several roles, each its own QQ account and
// project in this process, share a group. A person @-s the role they want; the lead hands work
// to a teammate by writing "@<teammate nickname>", which goes out as a real QQ @; the teammate's
// final answer @-s the lead back. Team mode is on when team_lead_bot is set, as on Discord.
//
// Differences from Discord, by platform limits:
//   - QQ shows no bot flag, so a teammate is recognised by being another qq platform of this
//     process (teamPlatforms), not by the sender.
//   - There is no gate input here: a person's group message reaches a role only when it @-s or
//     names that role; everything else in the group is ignored.
//   - allow_from must list people. Open ("" or "*") in team mode would let anyone in the group
//     run the roles' tools, so people are refused until it is set.

// teamPlatforms: QQ number → *Platform for every qq bot logged in in this process.
var teamPlatforms sync.Map

type teamState struct {
	lead         string // team_lead_bot: the primary role's QQ number ("" = not in team mode)
	nick         atomic.Pointer[string]
	openWarn     sync.Once
	leadReturned sync.Map // "<group>:<message>" → time: the first final chunk @-ed the lead
	groups       sync.Map // group ID string → struct{}: groups this account is in or heard from
}

const leadReturnTTL = 24 * time.Hour

func (p *Platform) teamMode() bool { return p.team.lead != "" }

func (p *Platform) botName() string {
	if n := p.team.nick.Load(); n != nil {
		return *n
	}
	return ""
}

func (p *Platform) selfIDString() string {
	if id := p.selfIDValue(); id != 0 {
		return strconv.FormatInt(id, 10)
	}
	return ""
}

// joinTeam registers this bot once it knows who it is, and learns its groups so a bare
// "qq:<id>" session key can be told apart from a private chat (ReconstructReplyCtx).
func (p *Platform) joinTeam(nick string) {
	p.team.nick.Store(&nick)
	if id := p.selfIDValue(); id != 0 {
		teamPlatforms.Store(id, p)
	}
	raw, err := p.callAPIRaw("get_group_list", nil)
	if err != nil {
		slog.Debug("qq: get_group_list failed", "error", err)
		return
	}
	for _, g := range decodeList(raw) {
		gid := jsonInt64(g, "group_id")
		if gid == 0 {
			continue
		}
		key := strconv.FormatInt(gid, 10)
		p.team.groups.Store(key, struct{}{})
		if name, _ := g["group_name"].(string); name != "" {
			p.groupNameCache.Store(key, name)
		}
	}
}

func (p *Platform) leaveTeam() {
	if id := p.selfIDValue(); id != 0 {
		teamPlatforms.CompareAndDelete(id, p)
	}
}

func (p *Platform) knownGroup(id string) bool {
	_, ok := p.team.groups.Load(id)
	return ok
}

// teammate returns the other bot of this process with QQ number id.
func (p *Platform) teammate(id int64) (*Platform, bool) {
	if id == 0 || id == p.selfIDValue() {
		return nil, false
	}
	v, ok := teamPlatforms.Load(id)
	if !ok {
		return nil, false
	}
	tp, _ := v.(*Platform)
	return tp, tp != nil
}

// teammateNames maps each other bot's nickname to its QQ number.
func (p *Platform) teammateNames() map[string]string {
	out := map[string]string{}
	self := p.selfIDValue()
	teamPlatforms.Range(func(k, v any) bool {
		id, _ := k.(int64)
		tp, _ := v.(*Platform)
		if id != 0 && id != self && tp != nil && tp.botName() != "" {
			out[tp.botName()] = strconv.FormatInt(id, 10)
		}
		return true
	})
	return out
}

func (p *Platform) teammateNameList() []string {
	names := make([]string, 0, 4)
	for n := range p.teammateNames() {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// groupText is a group message as the agent should read it: @-segments rendered as "@name".
// Its own @ is dropped, or kept as "@name(你)" when the message @-s someone else as well, so
// "@橘 @龙娘 一起看看" does not read to 橘 as addressed only to 龙娘 (Discord markSelfMention).
type groupText struct {
	text   string
	atSelf bool
	atLead bool
}

var cqAtRe = regexp.MustCompile(`\[CQ:at,qq=(\w+)[^\]]*\]`)

func (p *Platform) readGroupText(payload map[string]any) groupText {
	type part struct {
		text string
		at   string // QQ number or "all"; "" for text
	}
	var parts []part
	switch msg := payload["message"].(type) {
	case []any:
		for _, s := range msg {
			seg, _ := s.(map[string]any)
			data, _ := seg["data"].(map[string]any)
			switch seg["type"] {
			case "text":
				t, _ := data["text"].(string)
				parts = append(parts, part{text: t})
			case "at":
				parts = append(parts, part{at: fmt.Sprint(data["qq"])})
			}
		}
	default:
		raw, _ := payload["raw_message"].(string)
		last := 0
		for _, m := range cqAtRe.FindAllStringSubmatchIndex(raw, -1) {
			parts = append(parts, part{text: stripCQCodes(raw[last:m[0]])}, part{at: raw[m[2]:m[3]]})
			last = m[1]
		}
		parts = append(parts, part{text: stripCQCodes(raw[last:])})
	}

	self := p.selfIDString()
	var out groupText
	others := false
	for _, pt := range parts {
		switch {
		case pt.at == "":
		case pt.at == self:
			out.atSelf = true
		default:
			others = true
			if pt.at == p.team.lead {
				out.atLead = true
			}
		}
	}
	var b strings.Builder
	for _, pt := range parts {
		switch {
		case pt.at == "":
			b.WriteString(pt.text)
		case pt.at == self:
			if others {
				b.WriteString("@" + p.botName() + "(你)")
			}
		case pt.at == "all":
			b.WriteString("@全体成员")
		default:
			name := pt.at
			if id, err := strconv.ParseInt(pt.at, 10, 64); err == nil {
				if tp, ok := p.teammate(id); ok && tp.botName() != "" {
					name = tp.botName()
				}
			}
			b.WriteString("@" + name)
		}
	}
	out.text = strings.TrimSpace(b.String())
	return out
}

// handleTeamGroup turns a group message into a core message in team mode. The group is one
// shared session ("qq:<group>"), as Discord shares a channel, so the lead and the people it
// works with see the same conversation.
func (p *Platform) handleTeamGroup(payload map[string]any, userID, groupID, messageID int64, userName string, fromBot bool, images []core.ImageAttachment, files []core.FileAttachment, audio *core.AudioAttachment) {
	gid := strconv.FormatInt(groupID, 10)
	p.team.groups.Store(gid, struct{}{})
	in := p.readGroupText(payload)
	mentioned := in.atSelf || core.NameCalled(in.text, p.botName(), p.teammateNameList()...)
	if !mentioned {
		if !fromBot {
			slog.Debug("qq: team mode: group message does not call this bot", "group", groupID)
			return
		}
		// A teammate talking to someone else: context only. Core never starts a turn from it.
	}
	content := in.text
	if fromBot {
		if content == "" && len(images) == 0 && len(files) == 0 {
			return
		}
		content = "【" + userName + "】" + content
	} else if content == "" && len(images) == 0 && len(files) == 0 && audio == nil {
		// A bare @ means "look at what's going on" (Discord does the same).
		content = "（用户 @ 了你，在等你回复：看上面的频道消息判断要你做什么；看不出来就问）"
	}

	rctx := &replyContext{
		messageType: "group",
		userID:      userID,
		groupID:     groupID,
		messageID:   int32(messageID),
		lead:        p.dispatchedBy(fromBot, mentioned, userID, in.atLead),
	}
	msg := &core.Message{
		SessionKey: "qq:" + gid,
		Platform:   "qq",
		ChannelID:  gid,
		ChannelKey: gid,
		MessageID:  strconv.FormatInt(messageID, 10),
		UserID:     strconv.FormatInt(userID, 10),
		UserName:   userName,
		ChatName:   p.resolveGroupName(groupID),
		Content:    content,
		Images:     images,
		Files:      files,
		Audio:      audio,
		ReplyCtx:   rctx,
		Mentioned:  mentioned,
		FromBot:    fromBot,
	}
	slog.Debug("qq: team message", "group", groupID, "from_bot", fromBot, "mentioned", mentioned, "lead", rctx.lead)
	p.handler(p, msg)
}

// dispatchedBy is the lead whose hand-off this turn answers ("" = none): the lead @-ing this
// teammate, or a person @-ing this teammate and the lead together (Discord lead_return.go).
func (p *Platform) dispatchedBy(fromBot, mentioned bool, authorID int64, atLead bool) string {
	lead := p.team.lead
	if !mentioned || lead == "" || lead == p.selfIDString() {
		return ""
	}
	if fromBot {
		if strconv.FormatInt(authorID, 10) == lead {
			return lead
		}
		return ""
	}
	if atLead {
		return lead
	}
	return ""
}

// outgoing builds the OneBot segments for content. Text always goes out as text segments, never
// as a CQ string: NapCat parses CQ codes in a string message, so model output containing
// "[CQ:image,file=file:///…]" or "[CQ:at,qq=all]" would have been executed, not shown.
// "@<teammate nickname>" becomes a real @, and a teammate's final answer to a lead hand-off
// starts with an @ of the lead, so the lead is woken even if the model forgot to write it.
func (p *Platform) outgoing(rctx *replyContext, content string) []map[string]any {
	segs := linkMentions(content, p.teammateNames())
	if lead := p.leadToReturn(rctx, content, segs); lead != "" {
		segs = append([]map[string]any{atSegment(lead), textSegment(" ")}, segs...)
	}
	return segs
}

func (p *Platform) leadToReturn(rctx *replyContext, content string, segs []map[string]any) string {
	if rctx == nil || !rctx.final || rctx.lead == "" || strings.TrimSpace(content) == "" {
		return ""
	}
	for _, s := range segs {
		if s["type"] == "at" {
			if data, _ := s["data"].(map[string]any); data["qq"] == rctx.lead {
				return ""
			}
		}
	}
	now := time.Now()
	key := fmt.Sprintf("%d:%d", rctx.groupID, rctx.messageID)
	if _, seen := p.team.leadReturned.LoadOrStore(key, now); seen {
		return "" // a long answer goes out in chunks: only the first one @-s the lead
	}
	p.team.leadReturned.Range(func(k, v any) bool {
		if t, ok := v.(time.Time); ok && now.Sub(t) > leadReturnTTL {
			p.team.leadReturned.Delete(k)
		}
		return true
	})
	return rctx.lead
}

func textSegment(s string) map[string]any {
	return map[string]any{"type": "text", "data": map[string]any{"text": s}}
}

func atSegment(qq string) map[string]any {
	return map[string]any{"type": "at", "data": map[string]any{"qq": qq}}
}

// linkMentions splits content into text segments and @-segments for the given names (longest
// name first, so "龙娘二号" is not read as "龙娘"). Text inside ``` fences is left alone.
func linkMentions(content string, alias map[string]string) []map[string]any {
	names := make([]string, 0, len(alias))
	for n := range alias {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })

	var segs []map[string]any
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			segs = append(segs, textSegment(buf.String()))
			buf.Reset()
		}
	}
	for i, part := range strings.Split(content, "```") {
		if i > 0 {
			buf.WriteString("```")
		}
		if i%2 == 1 || len(names) == 0 { // inside a fence, or nobody to link
			buf.WriteString(part)
			continue
		}
		s := part
		for len(s) > 0 {
			idx := strings.IndexByte(s, '@')
			if idx < 0 {
				buf.WriteString(s)
				break
			}
			buf.WriteString(s[:idx])
			s = s[idx+1:]
			matched := false
			for _, n := range names {
				if strings.HasPrefix(s, n) {
					flush()
					segs = append(segs, atSegment(alias[n]))
					s = s[len(n):]
					matched = true
					break
				}
			}
			if !matched {
				buf.WriteByte('@')
			}
		}
	}
	flush()
	return segs
}

// ── core capability interfaces ───────────────────────────────────

var (
	_ core.AgentReplyMarker          = (*Platform)(nil)
	_ core.FinalReplyMarker          = (*Platform)(nil)
	_ core.InjectedReplyMerger       = (*Platform)(nil)
	_ core.SelfMentioner             = (*Platform)(nil)
	_ core.ChannelNameResolver       = (*Platform)(nil)
	_ core.ConfiguredTeamLead        = (*Platform)(nil)
	_ core.ConfiguredTeamLeadSelf    = (*Platform)(nil)
	_ core.ReplyContextReconstructor = (*Platform)(nil)
)

// MarkAgentReply implements core.AgentReplyMarker. The reply context is copied: core reuses one
// context for every send of a turn.
func (p *Platform) MarkAgentReply(ctx any) any {
	if rc, ok := ctx.(*replyContext); ok {
		c := *rc
		c.agent = true
		return &c
	}
	return ctx
}

// MarkFinalReply implements core.FinalReplyMarker.
func (p *Platform) MarkFinalReply(ctx any) any {
	if rc, ok := ctx.(*replyContext); ok {
		c := *rc
		c.final = true
		return &c
	}
	return ctx
}

// MergeInjectedReply implements core.InjectedReplyMerger: a lead's @ injected into a running turn
// marks that turn as lead-dispatched (Discord keepLead).
func (p *Platform) MergeInjectedReply(turnCtx, injectedCtx any) any {
	turn, ok := turnCtx.(*replyContext)
	if !ok || turn.lead != "" {
		return turnCtx
	}
	if in, ok := injectedCtx.(*replyContext); ok && in.lead != "" {
		c := *turn
		c.lead = in.lead
		return &c
	}
	return turnCtx
}

// SelfMention is how teammates address this bot: "@<nickname>", which their outgoing text turns
// into a real QQ @. Empty until login.
func (p *Platform) SelfMention() string {
	if n := p.botName(); n != "" {
		return "@" + n
	}
	return ""
}

// ResolveChannelName implements core.ChannelNameResolver for groups.
func (p *Platform) ResolveChannelName(channelID string) (string, error) {
	gid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("qq: channel %q is not a group", channelID)
	}
	return p.resolveGroupName(gid), nil
}

// HasConfiguredTeamLead reports whether team_lead_bot is set.
func (p *Platform) HasConfiguredTeamLead() bool { return p.team.lead != "" }

// IsConfiguredTeamLead reports whether team_lead_bot names this bot.
func (p *Platform) IsConfiguredTeamLead() bool {
	return p.team.lead != "" && p.team.lead == p.selfIDString()
}
