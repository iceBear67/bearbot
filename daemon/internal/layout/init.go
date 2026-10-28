package layout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultAgentSettings is written to <AgentDir>/settings.json on the first start (DESIGN 4.3).
const DefaultAgentSettings = `{
  "quietStartup": true,
  "defaultProjectTrust": "never"
}
`

// AgentsTemplate is written to the workspace when it is empty (DESIGN 4.4).
const AgentsTemplate = `# 记忆

## 用途

## 成员与称呼

## 约定与偏好

## 资料索引（docs/）

## 扩展、技能与 MCP
<!-- 每个自己编写的扩展一行：文件、用途、依赖、注意事项 -->

## 经验记录
<!-- 犯过的错误与修复方式 -->
`

// ClassifierTemplate is written to the workspace when it is empty (DESIGN 4.4).
const ClassifierTemplate = `# classifier 补充规则

<!-- 只有管理员发起的回合可以修改本文件。这里的规则在 member 回合中补充模型判断的标准，修改后下一轮生效。 -->

## 允许成员进行的操作

## 禁止成员进行的操作
`

// Owner tells who owns a directory or file that the daemon creates.
type Owner int

const (
	// OwnerRoot is root:root.
	OwnerRoot Owner = iota
	// OwnerBearbot is bearbot:bearbot.
	OwnerBearbot
	// OwnerGateway is bearbot-gw:bearbot-gw.
	OwnerGateway
	// OwnerRootGatewayGroup is root:bearbot-gw (the socket directory, where the gateway creates its sockets).
	OwnerRootGatewayGroup
)

// IDs returns the uid and gid of the owner.
func (u *Users) IDs(o Owner) (uid, gid uint32) {
	switch o {
	case OwnerBearbot:
		return u.Bearbot.UID, u.Bearbot.GID
	case OwnerGateway:
		return u.Gateway.UID, u.Gateway.GID
	case OwnerRootGatewayGroup:
		return u.Root.UID, u.Gateway.GID
	default:
		return u.Root.UID, u.Root.GID
	}
}

// DirSpec is a directory with the owner and mode the daemon enforces at startup.
type DirSpec struct {
	Path  string
	Owner Owner
	Mode  os.FileMode
}

// Dirs lists the directories of DESIGN 4.3-4.5 in creation order (parents first). The three mount points must
// exist; the others are created when missing. The Python virtual environment is handled separately.
func (p Paths) Dirs() []DirSpec {
	return []DirSpec{
		{p.ProfileDir, OwnerRoot, 0o755},
		{p.AgentDir, OwnerBearbot, 0o775},
		{p.ProfileBin, OwnerBearbot, 0o775},
		{p.SysDir, OwnerRoot, 0o755},
		{p.DaemonDir, OwnerRoot, 0o700},
		{p.ClassifierTurnDir, OwnerRoot, 0o700},
		{p.GatewayDataDir, OwnerGateway, 0o700},
		{p.LogsDir, OwnerRoot, 0o755},
		{p.AgentLogDir, OwnerRoot, 0o755},
		{p.RecoveryLogDir, OwnerRoot, 0o755},
		{p.IncidentsDir, OwnerRoot, 0o755},
		{p.RecoverySessionsDir, OwnerBearbot, 0o700},
		{p.ChecksDir, OwnerRoot, 0o755},
		{p.WorkspaceDir, OwnerBearbot, os.ModeSetgid | 0o770},
		{p.ConfigDir, OwnerRoot, 0o700},
		// The gateway (group bearbot-gw) creates its sockets here; bearbot may only enter it. The sticky bit keeps
		// the gateway from removing the daemon's sockets.
		{p.RunDir, OwnerRootGatewayGroup, os.ModeSticky | 0o771},
	}
}

// VenvDir is the DirSpec of the Python virtual environment directory.
func (p Paths) VenvDir() DirSpec { return DirSpec{p.PythonVenv, OwnerBearbot, 0o775} }

// InitResult tells what Init created.
type InitResult struct {
	CreatedDirs    []string
	WroteSettings  bool
	WroteTemplates bool
	CreatedVenv    bool
	// PythonVersion is sys.version of the virtual environment (equal to the image's interpreter).
	PythonVersion string
}

// Init runs DESIGN 5.2 step 2: check the mount points, create missing directories and correct owners and modes,
// write the default agent/settings.json on the first start, write the AGENTS.md and CLASSIFIER.md templates into
// an empty workspace, create the Python virtual environment on the first start, and check the Python version.
func Init(ctx context.Context, p Paths, u *Users) (*InitResult, error) {
	for _, mp := range []string{p.ProfileDir, p.WorkspaceDir, p.ConfigDir} {
		info, err := os.Lstat(mp)
		if err != nil {
			return nil, fmt.Errorf("挂载点 %s 无法访问：%w", mp, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("挂载点 %s 不是目录", mp)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.RunDir), 0o755); err != nil {
		return nil, fmt.Errorf("创建 %s 失败：%w", filepath.Dir(p.RunDir), err)
	}
	res := &InitResult{}
	for _, d := range p.Dirs() {
		created, err := EnsureDir(d, u)
		if err != nil {
			return nil, err
		}
		if created {
			res.CreatedDirs = append(res.CreatedDirs, d.Path)
		}
	}

	if _, err := os.Lstat(p.AgentSettingsFile); errors.Is(err, os.ErrNotExist) {
		if err := WriteNewFile(p.AgentSettingsFile, []byte(DefaultAgentSettings), 0o664, OwnerBearbot, u); err != nil {
			return nil, err
		}
		res.WroteSettings = true
	} else if err != nil {
		return nil, fmt.Errorf("无法访问 %s：%w", p.AgentSettingsFile, err)
	}

	empty, err := dirEmpty(p.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	if empty {
		if err := WriteNewFile(p.AgentsMD, []byte(AgentsTemplate), 0o664, OwnerBearbot, u); err != nil {
			return nil, err
		}
		if err := WriteNewFile(p.ClassifierMD, []byte(ClassifierTemplate), 0o664, OwnerBearbot, u); err != nil {
			return nil, err
		}
		res.WroteTemplates = true
	}

	created, err := ensureVenv(ctx, p, u)
	if err != nil {
		return nil, err
	}
	res.CreatedVenv = created
	version, err := CheckPython(ctx, p, u)
	if err != nil {
		return nil, err
	}
	res.PythonVersion = version
	return res, nil
}

// EnsureDir creates d.Path when it is missing (its parent must exist), then sets owner and mode. An existing entry
// that is not a directory (including a symlink) is an error. It reports whether the directory was created.
func EnsureDir(d DirSpec, u *Users) (bool, error) {
	created := false
	info, err := os.Lstat(d.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(d.Path, 0o700); err != nil {
			return false, fmt.Errorf("创建目录 %s 失败：%w", d.Path, err)
		}
		created = true
	case err != nil:
		return false, fmt.Errorf("无法访问 %s：%w", d.Path, err)
	case !info.IsDir():
		return false, fmt.Errorf("%s 应当是目录，实际是 %s", d.Path, describeMode(info.Mode()))
	}
	uid, gid := u.IDs(d.Owner)
	if err := u.ChownIDs(d.Path, uid, gid); err != nil {
		return false, err
	}
	if err := os.Chmod(d.Path, d.Mode); err != nil {
		return false, fmt.Errorf("修改 %s 的权限失败：%w", d.Path, err)
	}
	return created, nil
}

// WriteNewFile creates path (it must not exist; a final symlink is never followed), writes data, and sets mode and
// owner through the open descriptor, so nothing touches the path after it was created (in a directory bearbot owns,
// the entry may be replaced by a symlink meanwhile).
func WriteNewFile(path string, data []byte, mode os.FileMode, owner Owner, u *Users) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建 %s 失败：%w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	// The owner first, then the mode: a chown by root clears set-id bits.
	uid, gid := u.IDs(owner)
	if err := u.FchownIDs(f, uid, gid); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return fmt.Errorf("修改 %s 的权限失败：%w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	return nil
}

func dirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, fmt.Errorf("无法读取目录 %s：%w", dir, err)
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("无法读取目录 %s：%w", dir, err)
	}
	return false, nil
}

func describeMode(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "符号链接"
	case m.IsRegular():
		return "普通文件"
	case m.IsDir():
		return "目录"
	default:
		return "特殊文件"
	}
}
