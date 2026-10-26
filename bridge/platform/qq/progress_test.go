package qq

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fastProgress shortens the progress windows so a test sees several posts in well under a second.
func fastProgress(p *Platform) {
	p.progress.mu.Lock()
	p.progress.first, p.progress.gap, p.progress.max = 80*time.Millisecond, 120*time.Millisecond, 200*time.Millisecond
	p.progress.mu.Unlock()
}

func progressPosts(f *fakeOneBot, gid int64) []string {
	var out []string
	for _, s := range f.groupPosts(gid) {
		if t := sentText(s); strings.HasPrefix(t, "⏳") {
			out = append(out, t)
		}
	}
	return out
}

// A long turn: tool lines are not posted one by one; a progress line comes after the first
// window with the step count and the latest step, a second one only after more steps, and none
// after the answer.
func TestProgress_LongTurnPostsRareDigestsAndStopsAtTheAnswer(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	fastProgress(lead)
	rc := groupCtx()
	before := len(f.groupPosts(teamGroup))

	for i := 1; i <= 5; i++ {
		sendAs(t, lead, rc, "🔧 Bash: `step "+string(rune('0'+i))+"`\nmore output", false)
	}
	if n := len(f.groupPosts(teamGroup)) - before; n != 0 {
		t.Fatalf("%d interim posts went out one by one", n)
	}
	waitFor(t, "first progress line", func() bool { return len(progressPosts(f, teamGroup)) == 1 })
	first := progressPosts(f, teamGroup)[0]
	if !strings.Contains(first, "5 步") || !strings.Contains(first, "step 5") || strings.Contains(first, "more output") {
		t.Fatalf("progress line = %q, want the count and the latest step's first line", first)
	}

	time.Sleep(300 * time.Millisecond) // no new steps: no new line
	if n := len(progressPosts(f, teamGroup)); n != 1 {
		t.Fatalf("progress posted %d lines without new steps", n)
	}
	sendAs(t, lead, rc, "🔧 Bash: `step 6`", false)
	waitFor(t, "second progress line", func() bool { return len(progressPosts(f, teamGroup)) == 2 })

	sendAs(t, lead, rc, "@龙娘 做完了", true) // the answer
	time.Sleep(400 * time.Millisecond)
	if n := len(progressPosts(f, teamGroup)); n != 2 {
		t.Fatalf("progress kept posting after the answer: %d lines", n)
	}
	posts := f.groupPosts(teamGroup)
	if got := sentText(posts[len(posts)-1]); !strings.Contains(got, "做完了") {
		t.Fatalf("last post = %q, want the answer", got)
	}
}

// A turn that answers within the first window posts no progress at all.
func TestProgress_ShortTurnPostsOnlyTheAnswer(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	fastProgress(lead)
	before := len(f.groupPosts(teamGroup))
	sendAs(t, lead, groupCtx(), "🔧 Read: a.go", false)
	sendAs(t, lead, groupCtx(), "看完了，没问题", true)
	time.Sleep(300 * time.Millisecond)
	posts := f.groupPosts(teamGroup)[before:]
	if len(posts) != 1 || sentText(posts[0]) != "看完了，没问题" {
		t.Fatalf("posts = %d, want only the answer", len(posts))
	}
}

// A notice that is not agent output (an error, a status line) replaces pending progress.
func TestProgress_NoticeDropsPendingProgress(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	fastProgress(lead)
	sendAs(t, lead, groupCtx(), "🔧 Bash: go build", false)
	if err := lead.Send(t.Context(), groupCtx(), "❌ agent 出错了"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(progressPosts(f, teamGroup)); n != 0 {
		t.Fatalf("progress posted after an error notice: %d", n)
	}
}

func TestProgress_ModesAllAndOff(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int
	}{{"all", 3}, {"off", 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFakeOneBot(t)
			f.group(teamGroup, "开发群", leadQQ, personQQ)
			p, _ := startBot(t, f, leadQQ, "橘", map[string]any{"interim_messages": tc.mode})
			fastProgress(p)
			for i := 0; i < 3; i++ {
				sendAs(t, p, groupCtx(), "🔧 step", false)
			}
			time.Sleep(300 * time.Millisecond)
			if got := len(f.groupPosts(teamGroup)); got != tc.want {
				t.Fatalf("interim_messages=%s: %d posts, want %d", tc.mode, got, tc.want)
			}
		})
	}
}

// The window doubles after each post, up to the cap, so an hour-long task posts a handful.
func TestProgressBoard_GapDoublesUpToTheCap(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	b := newProgressBoard(func(*replyContext, string) error {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return nil
	})
	b.first, b.gap, b.max = 20*time.Millisecond, 40*time.Millisecond, 100*time.Millisecond
	rc := groupCtx()
	stop := make(chan struct{})
	go func() { // a step every 5ms: there is always something new to report
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				b.add(rc, "🔧 step")
			}
		}
	}()
	time.Sleep(650 * time.Millisecond)
	close(stop)
	b.done(rc)

	mu.Lock()
	defer mu.Unlock()
	// posts near 20, 60, 140, 240, 340, 440, 540, 640ms: gaps 40, 80, 100, 100, …
	if len(at) < 5 || len(at) > 9 {
		t.Fatalf("%d posts in 650ms, want 5–9 with a doubling, capped window", len(at))
	}
	for i := 3; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 80*time.Millisecond {
			t.Fatalf("gap %d = %v, want the cap (100ms) once it has doubled past it", i, gap)
		}
	}
}
