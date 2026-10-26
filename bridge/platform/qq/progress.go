package qq

import (
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
// With interim_messages = "digest" (default) those step lines (isStepLine) are folded into a rare
// progress line, while the agent's own text still goes out as it comes:
// none for a turn that ends within progressFirst, then one per window, the window doubling up to
// progressMaxGap, so an hour-long task posts about six. The turn's answer, or any notice that is
// not agent output (an error, a status line), drops what was not posted yet.
// "all" keeps the old one-message-per-step behaviour; "off" drops step lines.

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
	mu    sync.Mutex
	chats map[string]*chatProgress
	first time.Duration // tests shorten these
	gap   time.Duration
	max   time.Duration
	post  func(rctx *replyContext, text string) error
}

type chatProgress struct {
	rctx    *replyContext
	steps   int // interim posts folded this turn
	posted  int // steps already reported
	last    string
	gap     time.Duration
	timer   *time.Timer
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

// add folds one interim post of the running turn.
func (b *progressBoard) add(rc *replyContext, content string) {
	key := progressKey(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
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
}

// done drops the chat's unposted progress: the answer or a notice replaces it.
func (b *progressBoard) done(rc *replyContext) {
	key := progressKey(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.chats[key]; c != nil {
		c.timer.Stop()
		delete(b.chats, key)
	}
}

func (b *progressBoard) flush(key string, c *chatProgress) {
	b.mu.Lock()
	if b.chats[key] != c { // the turn ended meanwhile
		b.mu.Unlock()
		return
	}
	if c.steps == c.posted {
		// Nothing new since the last post (a long tool call): stay quiet, look again later.
		c.timer = time.AfterFunc(c.gap, func() { b.flush(key, c) })
		b.mu.Unlock()
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

// stop cancels every pending progress post (platform shutdown).
func (b *progressBoard) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, c := range b.chats {
		c.timer.Stop()
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
// (core/i18n.go MsgThinking, MsgTool, MsgToolResult).
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
	case rc.agent && !rc.final:
		return false // the agent's own text mid-turn: send it, the turn goes on
	default:
		p.progress.done(rc)
		return false
	}
}
