// Package layout knows every path of the container (contracts 1.1), the users the children run as (contracts 1.2),
// the command lines and environments of the children (contracts 2), and prepares the directories, default files
// and the Python virtual environment at startup (DESIGN 4.2-4.4, 4.7, 5.2 step 2).
package layout

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Paths holds the absolute paths of DESIGN.md resolved under a root prefix (contracts 1.1).
type Paths struct {
	Root string

	ProfileDir          string
	AgentDir            string
	AgentSessionsDir    string
	ProfileBin          string
	PythonVenv          string
	SysDir              string
	DaemonDir           string
	GatewayDataDir      string
	LogsDir             string
	IncidentsDir        string
	RecoverySessionsDir string
	ChecksDir           string
	SnapshotsGit        string
	SnapshotWorkTree    string
	WorkspaceDir        string
	ConfigDir           string
	RecoveryOverrideDir string

	RunDir           string
	GatewaySock      string
	GatewayCtlSock   string
	AgentSock        string
	AdminSock        string
	GatewayConfig    string
	RecoveryAgentDir string

	InstallDir              string
	GatewayMain             string
	BearbotIMExt            string
	RecoveryExt             string
	SDKFile                 string
	SkillsDir               string
	RecoverySkill           string
	RecoveryProfileTemplate string
	Tsc                     string
	ToolchainTypes          string

	PiBin       string
	PiPackage   string
	Python      string
	Uv          string
	BearbotHome string

	// Files and directories derived from the ones above.

	// DefaultConfigFile is the default of `run --config`: <R>/bearbot/config/bearbot.toml.
	DefaultConfigFile string
	// HooksDir is <ConfigDir>/hooks (maintenance scripts, DESIGN 4.5).
	HooksDir string
	// StateFile is <DaemonDir>/state.json.
	StateFile string
	// ClassifierTurnDir is <DaemonDir>/classifier-turn.
	ClassifierTurnDir string
	// GatewayDB is <GatewayDataDir>/gateway.db; ConsoleFilesDir is <GatewayDataDir>/console-files.
	GatewayDB       string
	ConsoleFilesDir string
	// Log files (contracts 1.1 LogsDir).
	DaemonLog          string
	GatewayLog         string
	AgentLogDir        string
	AgentStderrLog     string
	AgentRPCEventsLog  string
	AgentClassifierLog string
	AgentExitsLog      string
	RecoveryLogDir     string
	// AgentSettingsFile and AgentModelsFile are the profile files that belong to the restart effective paths.
	AgentSettingsFile string
	AgentModelsFile   string
	// AgentsMD and ClassifierMD are the memory and the classifier rules in the workspace.
	AgentsMD     string
	ClassifierMD string
	// PythonBin is the interpreter of the virtual environment.
	PythonBin string
}

// NewPaths resolves every path under root, which must be an absolute directory path ("/" in production).
func NewPaths(root string) (Paths, error) {
	if !filepath.IsAbs(root) {
		return Paths{}, fmt.Errorf("根目录 %q 必须是绝对路径", root)
	}
	root = filepath.Clean(root)
	j := func(p string) string { return filepath.Join(root, p) }
	p := Paths{
		Root:                    root,
		ProfileDir:              j("/bearbot/profile"),
		AgentDir:                j("/bearbot/profile/agent"),
		ProfileBin:              j("/bearbot/profile/bin"),
		PythonVenv:              j("/bearbot/profile/python"),
		SysDir:                  j("/bearbot/profile/bearbot"),
		DaemonDir:               j("/bearbot/profile/bearbot/daemon"),
		GatewayDataDir:          j("/bearbot/profile/bearbot/gateway"),
		LogsDir:                 j("/bearbot/profile/bearbot/logs"),
		IncidentsDir:            j("/bearbot/profile/bearbot/incidents"),
		RecoverySessionsDir:     j("/bearbot/profile/bearbot/recovery-sessions"),
		ChecksDir:               j("/bearbot/profile/bearbot/checks"),
		SnapshotsGit:            j("/bearbot/profile/bearbot/snapshots.git"),
		SnapshotWorkTree:        j("/bearbot"),
		WorkspaceDir:            j("/bearbot/workspace"),
		ConfigDir:               j("/bearbot/config"),
		RecoveryOverrideDir:     j("/bearbot/config/recovery"),
		RunDir:                  j("/run/bearbot"),
		GatewaySock:             j("/run/bearbot/gateway.sock"),
		GatewayCtlSock:          j("/run/bearbot/gateway-ctl.sock"),
		AgentSock:               j("/run/bearbot/agent.sock"),
		AdminSock:               j("/run/bearbot/admin.sock"),
		GatewayConfig:           j("/run/bearbot/gateway.json"),
		RecoveryAgentDir:        j("/run/bearbot/recovery/agent"),
		InstallDir:              j("/opt/bearbot"),
		GatewayMain:             j("/opt/bearbot/gateway/dist/main.js"),
		BearbotIMExt:            j("/opt/bearbot/extensions/bearbot-im"),
		RecoveryExt:             j("/opt/bearbot/extensions/bearbot-recovery"),
		SDKFile:                 j("/opt/bearbot/sdk/index.ts"),
		SkillsDir:               j("/opt/bearbot/skills"),
		RecoverySkill:           j("/opt/bearbot/skills/bearbot-recovery"),
		RecoveryProfileTemplate: j("/opt/bearbot/recovery-profile"),
		Tsc:                     j("/opt/bearbot/toolchain/node_modules/.bin/tsc"),
		ToolchainTypes:          j("/opt/bearbot/toolchain/node_modules/@types"),
		PiBin:                   j("/usr/local/bin/pi"),
		PiPackage:               j("/usr/local/lib/node_modules/@earendil-works/pi-coding-agent"),
		Python:                  j("/usr/local/bin/python3.14"),
		Uv:                      j("/usr/local/bin/uv"),
		BearbotHome:             j("/home/bearbot"),
		DefaultConfigFile:       j("/bearbot/config/bearbot.toml"),
		HooksDir:                j("/bearbot/config/hooks"),
	}
	p.AgentSessionsDir = filepath.Join(p.AgentDir, "sessions", EncodeSessionDir(p.WorkspaceDir))
	p.StateFile = filepath.Join(p.DaemonDir, "state.json")
	p.ClassifierTurnDir = filepath.Join(p.DaemonDir, "classifier-turn")
	p.GatewayDB = filepath.Join(p.GatewayDataDir, "gateway.db")
	p.ConsoleFilesDir = filepath.Join(p.GatewayDataDir, "console-files")
	p.DaemonLog = filepath.Join(p.LogsDir, "daemon.log")
	p.GatewayLog = filepath.Join(p.LogsDir, "gateway.log")
	p.AgentLogDir = filepath.Join(p.LogsDir, "agent")
	p.AgentStderrLog = filepath.Join(p.AgentLogDir, "stderr.log")
	p.AgentRPCEventsLog = filepath.Join(p.AgentLogDir, "rpc-events.jsonl")
	p.AgentClassifierLog = filepath.Join(p.AgentLogDir, "classifier.jsonl")
	p.AgentExitsLog = filepath.Join(p.AgentLogDir, "exits.jsonl")
	p.RecoveryLogDir = filepath.Join(p.LogsDir, "recovery")
	p.AgentSettingsFile = filepath.Join(p.AgentDir, "settings.json")
	p.AgentModelsFile = filepath.Join(p.AgentDir, "models.json")
	p.AgentsMD = filepath.Join(p.WorkspaceDir, "AGENTS.md")
	p.ClassifierMD = filepath.Join(p.WorkspaceDir, "CLASSIFIER.md")
	p.PythonBin = filepath.Join(p.PythonVenv, "bin", "python")
	return p, nil
}

// EncodeSessionDir returns pi's default session directory name for a cwd (contracts 1.1):
// "--" + the path without its leading "/" and with every "/" replaced by "-" + "--".
func EncodeSessionDir(cwd string) string {
	return "--" + strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-") + "--"
}

// ChildPATH is the PATH of every child: ProfileBin, the venv's bin, <R>/usr/local/bin, /usr/local/bin, /usr/bin,
// /bin, with duplicates removed in order (contracts 1.1).
func (p Paths) ChildPATH() string {
	var dirs []string
	for _, d := range []string{p.ProfileBin, filepath.Join(p.PythonVenv, "bin"), filepath.Join(p.Root, "/usr/local/bin"), "/usr/local/bin", "/usr/bin", "/bin"} {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, ":")
}

// IncidentDir returns <IncidentsDir>/<id>.
func (p Paths) IncidentDir(id string) string { return filepath.Join(p.IncidentsDir, id) }

// CheckDir returns <ChecksDir>/<id>.
func (p Paths) CheckDir(id string) string { return filepath.Join(p.ChecksDir, id) }

// RecoveryLog returns <LogsDir>/recovery/<incident id>.log.
func (p Paths) RecoveryLog(incidentID string) string {
	return filepath.Join(p.RecoveryLogDir, incidentID+".log")
}

// WorkTreeRel returns path relative to SnapshotWorkTree, or an error when path is outside of it.
func (p Paths) WorkTreeRel(path string) (string, error) {
	rel, err := filepath.Rel(p.SnapshotWorkTree, filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New(path + " 不在 " + p.SnapshotWorkTree + " 中")
	}
	return rel, nil
}
