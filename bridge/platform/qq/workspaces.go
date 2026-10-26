package qq

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Workspaces for QQ team mode (issue #344, step 4). QQ bots cannot create groups, so the pool is
// made of groups a person creates: core adopts every free group named 「工作区」/「工作区n」 that
// this account is in (core/team_workspace.go teamWorkspacePlan), and only asks to create one when
// none is free. Then the chat gets a note saying which group to make; the error is deliberately
// not ErrTeamWorkspacePermanent, which would block the chat until restart, while the fix here is
// "make the group and ask again".

var _ core.TeamWorkspaceChannels = (*Platform)(nil)

// workspaceNoteEvery limits the "please create the group" note to one per chat in this window.
const workspaceNoteEvery = 10 * time.Minute

var workspaceNotes sync.Map // "<self>:<group>" → time of the last note

// TeamGuildChannels implements core.TeamWorkspaceChannels. QQ has no guild: "the guild" is every
// group this account is in, all without a category.
func (p *Platform) TeamGuildChannels(ctx context.Context, channelID string) (core.TeamChannel, []core.TeamChannel, error) {
	if err := ctx.Err(); err != nil {
		return core.TeamChannel{}, nil, err
	}
	raw, err := p.callAPIRaw("get_group_list", nil)
	if err != nil {
		return core.TeamChannel{}, nil, fmt.Errorf("qq: workspace: list groups: %w", err)
	}
	var self core.TeamChannel
	found := false
	var all []core.TeamChannel
	for _, g := range decodeList(raw) {
		gid := jsonInt64(g, "group_id")
		if gid == 0 {
			continue
		}
		id := strconv.FormatInt(gid, 10)
		name, _ := g["group_name"].(string)
		p.team.groups.Store(id, struct{}{})
		if name != "" {
			p.groupNameCache.Store(id, name)
		}
		c := core.TeamChannel{ID: id, Name: name}
		all = append(all, c)
		if id == channelID {
			self, found = c, true
		}
	}
	if !found {
		return core.TeamChannel{}, nil, fmt.Errorf("qq: channel %s is not a group of this account: %w", channelID, core.ErrNotTeamChannel)
	}
	return self, all, nil
}

// CreateTeamWorkspaceChannel implements core.TeamWorkspaceChannels. A bot cannot create a QQ
// group, so it tells the chat which group to make and fails; the next request finds the group
// once it exists.
func (p *Platform) CreateTeamWorkspaceChannel(ctx context.Context, besideChannelID, name string) (core.TeamChannel, error) {
	gid, err := strconv.ParseInt(besideChannelID, 10, 64)
	if err != nil {
		return core.TeamChannel{}, fmt.Errorf("qq: workspace: channel %q is not a group", besideChannelID)
	}
	key := p.selfIDString() + ":" + besideChannelID
	now := time.Now()
	last, seen := workspaceNotes.Load(key)
	if t, ok := last.(time.Time); !seen || !ok || now.Sub(t) >= workspaceNoteEvery {
		workspaceNotes.Store(key, now)
		note := fmt.Sprintf("要开新的工作区，但 QQ 机器人不能自己建群。请建一个群，群名设成「%s」，把%s拉进去，然后再说一次。", name, p.workspaceMembersText())
		if err := p.Send(ctx, &replyContext{messageType: "group", groupID: gid}, note); err != nil {
			slog.Warn("qq: workspace: note failed", "group", gid, "error", err)
		}
	}
	return core.TeamChannel{}, fmt.Errorf("qq: workspace %q: QQ bots cannot create groups; create it by hand and add the team", name)
}

// workspaceMembersText names who must be in a workspace group: every role of this process.
func (p *Platform) workspaceMembersText() string {
	var names []string
	teamPlatforms.Range(func(k, v any) bool {
		id, _ := k.(int64)
		if tp, _ := v.(*Platform); tp != nil {
			n := tp.botName()
			if n == "" {
				n = strconv.FormatInt(id, 10)
			}
			names = append(names, fmt.Sprintf("%s（%d）", n, id))
		}
		return true
	})
	sort.Strings(names)
	if len(names) == 0 {
		return "所有角色的 QQ 号"
	}
	return "所有角色的 QQ 号 " + strings.Join(names, "、")
}
