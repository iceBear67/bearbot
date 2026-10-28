package layout

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"slices"
	"strconv"
	"syscall"
)

// User is a resolved account.
type User struct {
	Name string
	UID  uint32
	GID  uint32
	// Groups are the supplementary group ids.
	Groups []uint32
}

// Users holds the accounts of contracts 1.2. In rootless mode every account is the daemon's own user, children get
// no Credential and ownership changes are skipped (modes are still applied).
type Users struct {
	Rootless bool
	// Root owns the daemon's files (uid 0 / gid 0 in production).
	Root User
	// Bearbot runs the agent, the trial, recovery and the helper commands of the agent's files.
	Bearbot User
	// Gateway (bearbot-gw) runs the gateway; it has the supplementary group bearbot.
	Gateway User
	// BearbotGroup is the gid of the group bearbot (gateway.json socketGroupGid).
	BearbotGroup uint32
}

// LookupUsers resolves the users. Without rootless the daemon must run as root.
func LookupUsers(rootless bool) (*Users, error) {
	if rootless {
		cur, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("无法取得当前用户：%w", err)
		}
		groups, err := os.Getgroups()
		if err != nil {
			return nil, fmt.Errorf("无法取得当前用户的附加组：%w", err)
		}
		self := User{Name: cur.Username, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
		for _, g := range groups {
			self.Groups = append(self.Groups, uint32(g))
		}
		return &Users{Rootless: true, Root: self, Bearbot: self, Gateway: self, BearbotGroup: self.GID}, nil
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("bearbotd 必须以 root 身份运行；以普通用户在本地测试时使用 --rootless")
	}
	bearbot, err := lookupUser("bearbot")
	if err != nil {
		return nil, err
	}
	gw, err := lookupUser("bearbot-gw")
	if err != nil {
		return nil, err
	}
	group, err := user.LookupGroup("bearbot")
	if err != nil {
		return nil, fmt.Errorf("找不到用户组 bearbot：%w", err)
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("用户组 bearbot 的 gid %q 无效：%w", group.Gid, err)
	}
	if !slices.Contains(gw.Groups, uint32(gid)) && gw.GID != uint32(gid) {
		return nil, errors.New("用户 bearbot-gw 不在用户组 bearbot 中（需要附加组 bearbot）")
	}
	return &Users{
		Root:         User{Name: "root", UID: 0, GID: 0},
		Bearbot:      bearbot,
		Gateway:      gw,
		BearbotGroup: uint32(gid),
	}, nil
}

func lookupUser(name string) (User, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return User{}, fmt.Errorf("找不到用户 %s：%w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return User{}, fmt.Errorf("用户 %s 的 uid %q 无效：%w", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return User{}, fmt.Errorf("用户 %s 的 gid %q 无效：%w", name, u.Gid, err)
	}
	ids, err := u.GroupIds()
	if err != nil {
		return User{}, fmt.Errorf("无法取得用户 %s 的附加组：%w", name, err)
	}
	out := User{Name: name, UID: uint32(uid), GID: uint32(gid)}
	for _, id := range ids {
		g, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return User{}, fmt.Errorf("用户 %s 的组 id %q 无效：%w", name, id, err)
		}
		out.Groups = append(out.Groups, uint32(g))
	}
	return out, nil
}

// Credential returns the credential a child of user runs with; nil in rootless mode.
func (u *Users) Credential(who User) *syscall.Credential {
	if u.Rootless {
		return nil
	}
	return &syscall.Credential{Uid: who.UID, Gid: who.GID, Groups: append([]uint32{}, who.Groups...)}
}

// Chown sets the owner of path (without following a final symlink) to who's uid and primary gid. It does nothing in
// rootless mode.
func (u *Users) Chown(path string, who User) error {
	return u.ChownIDs(path, who.UID, who.GID)
}

// FchownIDs sets the owner of the open file f. It does nothing in rootless mode.
func (u *Users) FchownIDs(f *os.File, uid, gid uint32) error {
	if u.Rootless {
		return nil
	}
	if err := f.Chown(int(uid), int(gid)); err != nil {
		return fmt.Errorf("修改 %s 的属主失败：%w", f.Name(), err)
	}
	return nil
}

// ChownIDs sets the owner of path (without following a final symlink). It does nothing in rootless mode.
func (u *Users) ChownIDs(path string, uid, gid uint32) error {
	if u.Rootless {
		return nil
	}
	if err := os.Lchown(path, int(uid), int(gid)); err != nil {
		return fmt.Errorf("修改 %s 的属主失败：%w", path, err)
	}
	return nil
}
