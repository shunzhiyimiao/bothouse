package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A task workspace is an execution location, not a new task identity: it must
// belong to the source chat and have room for the work.
func TestResolveTaskWorkspaceChecksOwnershipAndOccupancy(t *testing.T) {
	e := NewEngine("source", &stubAgent{}, []Platform{newProgressPlatform()}, "", LangChinese)
	e.SetDataDir(t.TempDir())
	if err := e.ClaimTeamWorkspace("discord:room", "ws1", "工作区1"); err != nil {
		t.Fatal(err)
	}
	got, err := e.resolveTaskWorkspace("discord:room:user", "ws1")
	if err != nil || got != "discord:ws1" {
		t.Fatalf("own workspace = %q, %v", got, err)
	}
	if _, err := e.resolveTaskWorkspace("discord:room:user", "ws9"); err == nil || !strings.Contains(err.Error(), "not a recorded workspace") {
		t.Fatalf("unrecorded workspace = %v", err)
	}
	if _, err := e.resolveTaskWorkspace("discord:room:user", "feishu:ws1"); err == nil {
		t.Fatal("cross-platform workspace accepted")
	}
	if err := e.ClaimTeamWorkspace("discord:other", "ws2", "工作区2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolveTaskWorkspace("discord:room:user", "ws2"); err == nil || !strings.Contains(err.Error(), "another chat channel") {
		t.Fatalf("cross-channel workspace = %v", err)
	}

	busy := NewEngine("source", &stubAgent{}, []Platform{newProgressPlatform()}, "", LangChinese)
	busy.SetDataDir(t.TempDir())
	for i := 0; i < teamWorkspaceBusy; i++ {
		if err := busy.ClaimTeamWorkspace("discord:room", "ws1", "工作区1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := busy.resolveTaskWorkspace("discord:room:user", "ws1"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("full workspace = %v", err)
	}
}

func taskWorkspaceAssignment(id, run, member, workspace string) TaskMemberAssignment {
	return TaskMemberAssignment{
		ReporterProject: "target",
		Project:         "source",
		SessionKey:      "discord:room:user",
		Run:             run,
		TaskID:          id,
		MemberID:        member,
		Name:            "Target",
		Title:           "workspace member",
		Ref:             "无（新建）",
		Workspace:       workspace,
	}
}

// The member credential is scoped to the workspace, while its row and result
// stay on the source card.
func TestAssignMemberWorkspaceScopesCredentialToWorkspace(t *testing.T) {
	p, platform, run, id := taskRelayFixture(t)
	e := NewEngine("source", &stubAgent{}, []Platform{platform}, "", LangChinese)
	e.SetDataDir(p.dataDir)
	if err := e.ClaimTeamWorkspace("discord:room:user", "ws1", "工作区1"); err != nil {
		t.Fatal(err)
	}
	p.resolveWorkspace = e.resolveTaskWorkspace
	p.resolveReporter = func(project, platformName, session string) (TaskCardPublisher, error) {
		if project == "target" && platformName == "discord" && session == "discord:ws1" {
			return platform, nil
		}
		return nil, errors.New("unbound test reporter")
	}

	req := taskWorkspaceAssignment(id, run, "m1", "ws1")
	grant, err := p.AssignMember(req)
	if err != nil {
		t.Fatal(err)
	}
	if grant.ReportCommand != "" || !strings.Contains(grant.ReportVia, "task relay") {
		t.Fatalf("non-native member credential leaked to the creator: %+v", grant)
	}
	p.mu.Lock()
	var command string
	for _, g := range p.memberRuns {
		if g.id == "m1" {
			command = p.memberReportCommand(g)
		}
	}
	p.mu.Unlock()
	if !strings.Contains(command, "--session 'discord:ws1'") {
		t.Fatalf("report command not scoped to workspace: %s", command)
	}
	task, ok := p.Task(id)
	if !ok || len(task.Members) != 1 || task.Members[0].Workspace != "discord:ws1" {
		t.Fatalf("member row = %+v", task)
	}
	token := progressToken(t, command)
	if _, err := p.Report(PublicProgressRequest{Project: "source", SessionKey: "discord:room:user", Run: token, Seq: 1, Kind: "update", Summary: "wrong chat"}); err == nil {
		t.Fatal("report from the source chat accepted for a workspace member")
	}
	if _, err := p.Report(PublicProgressRequest{Project: "source", SessionKey: "discord:ws1", Run: token, Seq: 1, Kind: "update", Summary: "working in workspace"}); err != nil {
		t.Fatalf("workspace report: %v", err)
	}
	retry, err := p.AssignMember(req)
	if err != nil || retry.ReportCommand != grant.ReportCommand || retry.ReportVia != grant.ReportVia {
		t.Fatalf("idempotent retry = %+v, %v", retry, err)
	}
	task, _ = p.Task(id)
	if len(task.Members) != 1 {
		t.Fatalf("retry added a member row: %+v", task.Members)
	}

	// The workspace survives a fresh process reading the same data dir.
	restored, err := NewPublicProgress(p.dataDir, "source", []Platform{platform})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, ok := restored.Task(id)
	if !ok || len(reloaded.Members) != 1 || reloaded.Members[0].Workspace != "discord:ws1" {
		t.Fatalf("restored member = %+v", reloaded)
	}
}

// A workspace of another chat, or one this chat never claimed, is not a valid
// execution target.
func TestAssignMemberWorkspaceRejectsForeignAndUnrecorded(t *testing.T) {
	p, platform, run, id := taskRelayFixture(t)
	e := NewEngine("source", &stubAgent{}, []Platform{platform}, "", LangChinese)
	e.SetDataDir(p.dataDir)
	if err := e.ClaimTeamWorkspace("discord:other", "ws2", "工作区2"); err != nil {
		t.Fatal(err)
	}
	p.resolveWorkspace = e.resolveTaskWorkspace
	p.resolveReporter = func(project, platformName, session string) (TaskCardPublisher, error) {
		return platform, nil
	}
	for _, tc := range []struct{ workspace, want string }{
		{"ws2", "another chat channel"},
		{"ws9", "not a recorded workspace"},
	} {
		req := taskWorkspaceAssignment(id, run, "m-"+tc.workspace, tc.workspace)
		if _, err := p.AssignMember(req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("workspace %s = %v, want %q", tc.workspace, err, tc.want)
		}
	}
	task, _ := p.Task(id)
	if len(task.Members) != 0 {
		t.Fatalf("rejected assignment created rows: %+v", task.Members)
	}
}

// The target runs in the workspace session, while the receipt and final
// result stay on the source task; retries and conflicting targets are
// resolved from that receipt.
func TestTaskRelayWorkspaceExecutesThereAndReturnsResult(t *testing.T) {
	p, platform, _, id := taskRelayFixture(t)
	e := NewEngine("source", &stubAgent{}, []Platform{platform}, "", LangChinese)
	e.SetDataDir(p.dataDir)
	if err := e.ClaimTeamWorkspace("discord:room:user", "ws1", "工作区1"); err != nil {
		t.Fatal(err)
	}
	if err := e.ClaimTeamWorkspace("discord:room:user", "ws2", "工作区2"); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	rm, session := taskRelayManager(t, p, platform, nil, &starts)
	source := rm.engines["source"]
	source.dataDir = p.dataDir
	p.resolveWorkspace = source.resolveTaskWorkspace
	rm.Bind("discord", "ws1", map[string]string{"source": "Source", "target": "Target"})
	rm.Bind("discord", "ws2", map[string]string{"source": "Source", "target": "Target"})
	session.events <- Event{Type: EventResult, Content: "workspace result"}

	req := taskRelayRequest(id)
	req.Workspace = "ws1"
	result, err := rm.Send(context.Background(), req)
	if err != nil || result.Response != "workspace result" {
		t.Fatalf("relay = %+v, %v", result, err)
	}
	scope := id + ":" + fmt.Sprintf("%x", sha256.Sum256([]byte(req.RequestID)))
	wsKey := relayConversationKey("source", "discord", "ws1") + ":task:" + scope
	target := rm.engines["target"]
	if target.sessions.ActiveSessionID(wsKey) == "" {
		t.Fatal("target did not run in the workspace session")
	}
	if target.sessions.ActiveSessionID(relayConversationKey("source", "discord", "room")+":task:"+scope) != "" {
		t.Fatal("target also ran in the source chat session")
	}
	task, _ := p.Task(id)
	found := false
	for _, ev := range task.Events {
		if strings.Contains(ev.Detail, "workspace result") {
			found = true
		}
	}
	if !found {
		t.Fatalf("result not returned to the source task: %+v", task.Events)
	}
	rec := readProgressRecord(t, p.dir, id)
	if rec.Relays[req.RequestID].Workspace != "discord:ws1" {
		t.Fatalf("receipt workspace = %q", rec.Relays[req.RequestID].Workspace)
	}

	if cached, err := rm.Send(context.Background(), req); err != nil || cached.Response != result.Response {
		t.Fatalf("retry = %+v, %v", cached, err)
	}
	conflict := req
	conflict.Workspace = "ws2"
	if _, err := rm.Send(context.Background(), conflict); err == nil || !strings.Contains(err.Error(), "already dispatched to workspace") {
		t.Fatalf("conflicting target = %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("retry or conflict reran target: %d starts", starts.Load())
	}

	// A restart reloads the receipt and returns the durable result without running again.
	restored, err := NewPublicProgress(p.dataDir, "source", []Platform{platform})
	if err != nil {
		t.Fatal(err)
	}
	restored.resolveWorkspace = source.resolveTaskWorkspace
	rm.engines["source"].publicProgress = restored
	if cached, err := rm.Send(context.Background(), req); err != nil || cached.Response != result.Response {
		t.Fatalf("restart replay = %+v, %v", cached, err)
	}
	if starts.Load() != 1 {
		t.Fatalf("restart replay reran target: %d starts", starts.Load())
	}
}

// A target that is not bound in the workspace cannot receive its assignment.
func TestTaskRelayWorkspaceRequiresTargetBinding(t *testing.T) {
	p, platform, _, id := taskRelayFixture(t)
	e := NewEngine("source", &stubAgent{}, []Platform{platform}, "", LangChinese)
	e.SetDataDir(p.dataDir)
	if err := e.ClaimTeamWorkspace("discord:room:user", "ws1", "工作区1"); err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	rm, _ := taskRelayManager(t, p, platform, nil, &starts)
	source := rm.engines["source"]
	source.dataDir = p.dataDir
	p.resolveWorkspace = source.resolveTaskWorkspace
	req := taskRelayRequest(id)
	req.Workspace = "ws1"
	if _, err := rm.Send(context.Background(), req); err == nil || !strings.Contains(err.Error(), "没绑定") {
		t.Fatalf("unbound workspace target = %v", err)
	}
	if starts.Load() != 0 {
		t.Fatal("unbound target started")
	}
	if _, ok := p.records[id].record.Relays[req.RequestID]; ok {
		t.Fatal("failed binding check left a receipt")
	}
}

func readProgressRecord(t *testing.T, dir, id string) progressRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec progressRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}
