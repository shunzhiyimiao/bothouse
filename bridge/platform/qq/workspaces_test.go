package qq

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type nopAgent struct{}

func (nopAgent) Name() string { return "nop" }
func (nopAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return nil, errors.New("nop agent")
}
func (nopAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (nopAgent) Stop() error                                                   { return nil }

// leadEngine is the lead's engine with team workspaces on and a session in the team group.
func leadEngine(t *testing.T, lead *Platform) *core.Engine {
	t.Helper()
	e := core.NewEngine("lead", nopAgent{}, []core.Platform{lead}, "", core.LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetDataDir(t.TempDir())
	e.SetTeam(t.TempDir(), "lead", true) // inject: this project is the primary role
	e.SetTeamWorkspaces(true)
	e.GetSessions().GetOrCreateActive("qq:500")
	return e
}

func TestTeamGuildChannels_ListsTheAccountsGroups(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	f.group(600, "工作区1", leadQQ, subQQ, personQQ)
	f.group(700, "别人的群", subQQ)

	self, all, err := lead.TeamGuildChannels(t.Context(), "500")
	if err != nil {
		t.Fatal(err)
	}
	if self.ID != "500" || self.Name != "开发群" {
		t.Fatalf("self = %+v", self)
	}
	ids := map[string]string{}
	for _, c := range all {
		ids[c.ID] = c.Name
	}
	if len(ids) != 2 || ids["600"] != "工作区1" {
		t.Fatalf("groups = %v, want 500 and 600 (not 700, which the lead is not in)", ids)
	}
	if !lead.knownGroup("600") {
		t.Fatal("a listed group must be known, so qq:600 reconstructs as a group")
	}
	if _, _, err := lead.TeamGuildChannels(t.Context(), "12345"); !errors.Is(err, core.ErrNotTeamChannel) {
		t.Fatalf("private chat: err = %v, want ErrNotTeamChannel", err)
	}
}

// The whole workspace cycle on the real engine: a group the user made as 「工作区1」 is adopted;
// when none is free the chat is told which group to make (once, not on every retry), and once the
// user made it, asking again adopts it without a restart.
func TestWorkspaces_AdoptPreCreatedGroupsAndAskForTheNextOne(t *testing.T) {
	f, lead, _, _, _ := startTeam(t, "9")
	f.group(600, "工作区1", leadQQ, subQQ, personQQ)
	e := leadEngine(t, lead)

	_, w, err := e.NewTeamWorkspace("qq:500")
	if err != nil || w.ID != "600" {
		t.Fatalf("first workspace = %+v, %v; want the pre-created group 600", w, err)
	}

	before := len(f.groupPosts(500))
	if _, _, err := e.NewTeamWorkspace("qq:500"); err == nil {
		t.Fatal("no free group left, but a workspace was made")
	}
	if _, _, err := e.NewTeamWorkspace("qq:500"); err == nil {
		t.Fatal("second try without the group succeeded")
	}
	notes := f.groupPosts(500)[before:]
	if len(notes) != 1 || !strings.Contains(sentText(notes[0]), "「工作区2」") || !strings.Contains(sentText(notes[0]), "龙娘（2）") {
		t.Fatalf("notes = %d (%v), want one note naming 工作区2 and the roles", len(notes), notes)
	}

	f.group(601, "工作区2", leadQQ, subQQ, personQQ)
	_, w, err = e.NewTeamWorkspace("qq:500")
	if err != nil || w.ID != "601" {
		t.Fatalf("after creating 工作区2: %+v, %v; want it adopted without a restart", w, err)
	}
	if rc, _ := lead.ReconstructReplyCtx("qq:601"); rc.(*replyContext).messageType != "group" {
		t.Fatalf("qq:601 reconstructs as %+v, want the workspace group", rc)
	}
}
