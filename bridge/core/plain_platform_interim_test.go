package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// plainMarkingPlatform is a platform that cannot edit a sent message (no MessageUpdater,
// no PreviewStarter), like QQ over OneBot. It marks agent output and final replies, and
// records every send with the marks its reply context carried (issue #344, step 1).
type plainMarkingPlatform struct {
	stubPlatformEngine
	sendMu sync.Mutex
	sends  []plainSend
}

type plainSend struct {
	content string
	agent   bool
	final   bool
}

type plainMark struct {
	inner any
	agent bool
	final bool
}

func asPlainMark(ctx any) plainMark {
	if m, ok := ctx.(plainMark); ok {
		return m
	}
	return plainMark{inner: ctx}
}

func (p *plainMarkingPlatform) MarkAgentReply(ctx any) any {
	m := asPlainMark(ctx)
	m.agent = true
	return m
}

func (p *plainMarkingPlatform) MarkFinalReply(ctx any) any {
	m := asPlainMark(ctx)
	m.final = true
	return m
}

func (p *plainMarkingPlatform) Send(ctx context.Context, rctx any, content string) error {
	m, _ := rctx.(plainMark)
	p.sendMu.Lock()
	p.sends = append(p.sends, plainSend{content: content, agent: m.agent, final: m.final})
	p.sendMu.Unlock()
	return p.stubPlatformEngine.Send(ctx, rctx, content)
}

func (p *plainMarkingPlatform) Reply(ctx context.Context, rctx any, content string) error {
	return p.Send(ctx, rctx, content)
}

func (p *plainMarkingPlatform) recorded() []plainSend {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return append([]plainSend(nil), p.sends...)
}

// On a platform that cannot edit messages, the default display sends every thinking line and
// every tool call as a message of its own. The platform can only keep a group chat readable
// if it can tell those interim posts from the turn's answer: interim agent output must carry
// the agent mark without the final mark, and only the answer may carry the final mark.
func TestPlainPlatform_InterimAgentOutputIsMarkedButNotFinal(t *testing.T) {
	p := &plainMarkingPlatform{stubPlatformEngine: stubPlatformEngine{n: "plain"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	sessionKey := "plain:group1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx-1"}
	e.interactiveStates[sessionKey] = state

	go func() {
		agentSession.events <- Event{Type: EventThinking, Content: "planning the change"}
		for _, cmd := range []string{"ls", "go test ./...", "git diff"} {
			agentSession.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: cmd}
			agentSession.events <- Event{Type: EventToolResult, ToolName: "Bash", ToolResult: "ok"}
		}
		agentSession.events <- Event{Type: EventText, Content: "all three steps passed"}
		agentSession.events <- Event{Type: EventResult, Content: "all three steps passed", Done: true}
	}()
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, nil)

	sends := p.recorded()
	var interim, final []plainSend
	for _, s := range sends {
		if !s.agent {
			continue
		}
		if s.final {
			final = append(final, s)
		} else {
			interim = append(interim, s)
		}
	}
	// One thinking line plus one line per tool call, each its own message: this is the
	// flood the QQ platform has to absorb.
	if len(interim) < 4 {
		t.Fatalf("interim agent sends = %d (%#v), want ≥4: one per thinking line and tool call", len(interim), sends)
	}
	for _, s := range interim {
		if strings.Contains(s.content, "all three steps passed") {
			t.Fatalf("answer went out as interim output: %#v", s)
		}
	}
	if len(final) != 1 || !strings.Contains(final[0].content, "all three steps passed") {
		t.Fatalf("final sends = %#v, want exactly the answer", final)
	}
}

// Platforms that cannot edit messages tell step lines from the agent's own text by these
// prefixes (platform/qq progress.go isStepLine). Changing a template's prefix silently turns
// every tool line on QQ back into a message of its own.
func TestStepLinePrefixesArePinned(t *testing.T) {
	want := map[MsgKey]string{MsgThinking: "💭", MsgTool: "🔧", MsgToolResult: "📤"}
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		i := NewI18n(lang)
		for key, prefix := range want {
			if got := i.T(key); !strings.HasPrefix(got, prefix) {
				t.Errorf("%s %v = %q, want prefix %q", lang, key, got, prefix)
			}
		}
	}
}
