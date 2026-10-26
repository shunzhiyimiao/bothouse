package qq

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Interim output on QQ (issue #344, step 5). QQ cannot edit a sent message, so core sends every
// thinking line and tool call of a turn as a message of its own (core test
// TestPlainPlatform_InterimAgentOutputIsMarkedButNotFinal); in a group that buries the answer.
// With interim_messages = "digest" (default in team mode) those step lines (isStepLine) are
// folded into a rare progress line, while the agent's own text still goes out as it comes:
// none for a turn that ends within progressFirst, then one per window, the window doubling up
// to progressMaxGap, and none while nothing new happened, so an hour-long task posts about six.
//
// A turn's bounds come from core's typing hook (StartTyping is called when a turn starts and
// its stop when the turn ends, also between queued turns), not from the answer: core sends no
// answer at all on several normal paths (text already out before the last tool call, a silent
// reply), and progress must not outlive the turn.
//
// "all" keeps one message per step (default outside team mode); "off" drops step lines.

const (
	interimDigest = "digest"
	interimAll    = "all"
	interimOff    = "off"

	progressFirst  = 30 * time.Second
	progressGap    = time.Minute
	progressMaxGap = 10 * time.Minute
	progressLine   = 60 // runes of the latest step shown
)

type progressBoard struct {
	mu      sync.Mutex
	chats   map[string]*chatProgress
	stopped bool
	first   time.Duration // tests shorten these
	gap     time.Duration
	max     time.Duration
	post    func(rctx *replyContext, text string) error
}

type chatProgress struct {
	rctx    *replyContext
	steps   int // step lines folded this turn
	posted  int // steps already reported
	last    string
	gap     time.Duration
	timer   *time.Timer // nil while nothing is scheduled
	started time.Time
}

func newProgressBoard(post func(*replyContext, string) error) *progressBoard {
	return &progressBoard{chats: map[string]*chatProgress{}, first: progressFirst, gap: progressGap, max: progressMaxGap, post: post}
}

func progressKey(rc *replyContext) string {
	if rc.messageType == "group" {
		return fmt.Sprintf("g:%d", rc.groupID)
	}
	return fmt.Sprintf("u:%d", rc.userID)
}

// add folds one step line of the running turn.
func (b *progressBoard) add(rc *replyContext, content string) {
	key := progressKey(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	c := b.chats[key]
	if c == nil {
		c = &chatProgress{gap: b.gap, started: time.Now()}
		b.chats[key] = c
		c.timer = time.AfterFunc(b.first, func() { b.flush(key, c) })
	}
	c.rctx = rc
	c.steps++
	if line := firstLine(content); line != "" {
		c.last = line
	}
	if c.timer == nil { // went quiet after a post: report again one window from now
		c.timer = time.AfterFunc(c.gap, func() { b.flush(key, c) })
	}
}

// done ends the chat's turn: what was not posted is dropped.
func (b *progressBoard) done(rc *replyContext) {
	key := progressKey(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.chats[key]; c != nil {
		if c.timer != nil {
			c.timer.Stop()
		}
		delete(b.chats, key)
	}
}

func (b *progressBoard) flush(key string, c *chatProgress) {
	b.mu.Lock()
	if b.stopped || b.chats[key] != c { // stopped, or the turn ended meanwhile
		b.mu.Unlock()
		return
	}
	c.timer = nil
	if c.steps == c.posted {
		b.mu.Unlock() // nothing new (a long tool call): stay quiet until the next step
		return
	}
	text := fmt.Sprintf("⏳ 进行中 · 已 %s · %d 步 · 最近：%s", elapsed(time.Since(c.started)), c.steps, c.last)
	rc := *c.rctx
	rc.agent, rc.final, rc.lead = false, false, ""
	c.posted = c.steps
	c.timer = time.AfterFunc(c.gap, func() { b.flush(key, c) })
	c.gap *= 2
	if c.gap > b.max {
		c.gap = b.max
	}
	b.mu.Unlock()
	if err := b.post(&rc, text); err != nil {
		slog.Warn("qq: progress post failed", "chat", key, "error", err)
	}
}

// stop cancels every pending progress post for good (platform shutdown).
func (b *progressBoard) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	for key, c := range b.chats {
		if c.timer != nil {
			c.timer.Stop()
		}
		delete(b.chats, key)
	}
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if utf8.RuneCountInString(l) > progressLine {
				l = string([]rune(l)[:progressLine]) + "…"
			}
			return l
		}
	}
	return ""
}

func elapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// stepPrefixes open core's thinking, tool-call and tool-result lines in every language
// (core/i18n.go MsgThinking, MsgTool, MsgToolResult; pinned by core's
// TestStepLinePrefixesArePinned).
var stepPrefixes = []string{"💭", "🔧", "📤"}

// isStepLine: content is a thinking or tool line, not the agent's own text. Only these are
// folded. Interim text must still go out: when a turn's text all went out before its last tool
// call, core sends no final answer (core/engine.go "only send the unsent remainder"), and
// folding that text would lose the answer. Anything unrecognised is sent.
func isStepLine(content string) bool {
	s := strings.TrimSpace(content)
	for _, pre := range stepPrefixes {
		if strings.HasPrefix(s, pre) {
			return true
		}
	}
	return false
}

// interim decides what Send does with content: true = it was folded or dropped, do not send.
// Notices mid-turn (a permission prompt, "inserted") are sent and leave the turn's progress be;
// the answer ends it.
func (p *Platform) interim(rc *replyContext, content string) bool {
	switch {
	case p.progress == nil || p.interimMode == interimAll:
		return false
	case rc.agent && !rc.final && isStepLine(content):
		if p.interimMode == interimOff {
			return true
		}
		p.progress.add(rc, content)
		return true
	case rc.final:
		p.progress.done(rc)
		return false
	default:
		return false
	}
}

// StartTyping implements core.TypingIndicator. QQ shows no typing state; the hook marks a
// turn's bounds. The returned stop runs when the turn ends: progress that was not posted is
// dropped, and a teammate's turn handed over by the lead that never @-ed the lead back (its
// answer went out before the last tool call, or there was none) @-s the lead now, so the
// hand-off cannot stall silently.
func (p *Platform) StartTyping(_ context.Context, replyCtx any) (stop func()) {
	rc, ok := replyCtx.(*replyContext)
	if !ok {
		return func() {}
	}
	if p.progress != nil {
		p.progress.done(rc) // a new turn starts from zero
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if p.progress != nil {
				p.progress.done(rc)
			}
			p.returnToLeadAtTurnEnd(rc)
		})
	}
}

func (p *Platform) returnToLeadAtTurnEnd(rc *replyContext) {
	if rc.lead == "" || rc.messageType != "group" || !p.claimLeadReturn(rc) {
		return
	}
	segs := []map[string]any{atSegment(rc.lead), textSegment(" 这一轮结束了，结果见上面我的消息。")}
	if _, err := p.callAPI("send_group_msg", map[string]any{"group_id": rc.groupID, "message": segs}); err != nil {
		slog.Warn("qq: turn-end return to the lead failed", "group", rc.groupID, "error", err)
	}
}
