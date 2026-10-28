package layout

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"bearbot/daemon/internal/config"
)

// ToolsFlag is the --tools value of the agent, the trial and recovery.
const ToolsFlag = "+grep,+find,+ls"

// AgentArgs returns the arguments of pi for the agent process (contracts 2.1); the process runs as
// `<node> <PiBin> <AgentArgs>` and the cwd is WorkspaceDir.
func (p Paths) AgentArgs(sessionID string) []string {
	return []string{"--mode", "rpc", "--session-id", sessionID, "--session-dir", p.AgentSessionsDir, "--tools", ToolsFlag, "-e", p.BearbotIMExt}
}

// TrialArgs returns the arguments of pi for the trial process (contracts 2.2), run as `<node> <PiBin> <TrialArgs>`;
// the cwd is WorkspaceDir.
func (p Paths) TrialArgs() []string {
	return []string{"--mode", "rpc", "--no-session", "--tools", ToolsFlag, "-e", p.BearbotIMExt}
}

// RecoveryArgs returns the arguments of pi for the recovery process (contracts 2.3), run as
// `<node> <PiBin> <RecoveryArgs>`; the cwd is RecoveryCwd.
func (p Paths) RecoveryArgs(incidentID string) []string {
	return []string{
		"--mode", "rpc", "-ne", "-e", p.RecoveryExt, "--tools", ToolsFlag,
		"-ns", "--skill", p.RecoverySkill, "-np", "-nc", "-na",
		"--session-dir", p.RecoverySessionsDir, "--session-id", incidentID,
	}
}

// RecoveryCwd is the working directory of recovery: <R>/bearbot.
func (p Paths) RecoveryCwd() string { return p.SnapshotWorkTree }

// GatewayArgs returns argv[1:] of the gateway (contracts 2.4); argv[0] is node found in the daemon's PATH and the
// cwd is GatewayCwd.
func (p Paths) GatewayArgs() []string {
	return []string{p.GatewayMain, "--config", p.GatewayConfig}
}

// GatewayCwd is the working directory of the gateway: <InstallDir>/gateway.
func (p Paths) GatewayCwd() string { return filepath.Join(p.InstallDir, "gateway") }

// LoginArgs returns the arguments of pi for `bearbotd login` (contracts 2.5), run as `<node> <PiBin> <LoginArgs>`;
// the cwd is BearbotHome.
func (p Paths) LoginArgs() []string { return []string{"-ne"} }

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// inherited returns the daemon's TZ and LANG entries, each only when set.
func inherited() []string {
	var out []string
	for _, k := range []string{"TZ", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func appendMap(env []string, m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+m[k])
	}
	return env
}

func (p Paths) gitSafeDirectory() []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=" + p.SnapshotsGit}
}

// NoJitiCache turns off the file cache of jiti, the loader pi uses for extensions. Its cache lives in
// os.tmpdir()/jiti, bearbot can write there, and jiti accepts an entry without checking who wrote it, so an entry
// written by the agent would replace the code of bearbot-im, bearbot-recovery or the SDK.
const NoJitiCache = "JITI_FS_CACHE=false"

// AgentEnv returns the exact environment of the agent process (contracts 2.1). Login uses it too.
func (p Paths) AgentEnv(cfg *config.Config) []string {
	env := []string{
		"HOME=" + p.BearbotHome,
		"PATH=" + p.ChildPATH(),
		"VIRTUAL_ENV=" + p.PythonVenv,
		"UV_LINK_MODE=copy",
		"PI_CODING_AGENT_DIR=" + p.AgentDir,
		"PI_SKIP_VERSION_CHECK=1",
		NoJitiCache,
		"BEARBOT_WORKSPACE_DIR=" + p.WorkspaceDir,
		"BEARBOT_GATEWAY_SOCKET=" + p.GatewaySock,
		"BEARBOT_AGENT_SOCKET=" + p.AgentSock,
		"BEARBOT_CLASSIFIER_MODEL=" + cfg.Classifier.Model,
		"BEARBOT_CLASSIFIER_TIMEOUT=" + ms(cfg.Classifier.Timeout),
		"BEARBOT_CLASSIFIER_RULES_MAX_BYTES=" + strconv.FormatInt(cfg.Classifier.RulesMaxBytes, 10),
		"BEARBOT_OBSERVE_LIMIT=" + strconv.Itoa(cfg.Bridge.ObserveLimit),
		"BEARBOT_HOLD_WARN_AFTER=" + ms(cfg.Bridge.HoldWarnAfter),
		"BEARBOT_ERROR_NOTICE=" + cfg.Bridge.ErrorNotice,
	}
	env = append(env, p.gitSafeDirectory()...)
	env = append(env, inherited()...)
	return appendMap(env, cfg.Agent.Env)
}

// TrialEnv returns the environment of the trial process: the agent environment plus BEARBOT_TRIAL=1 (contracts 2.2).
func (p Paths) TrialEnv(cfg *config.Config) []string {
	return append(p.AgentEnv(cfg), "BEARBOT_TRIAL=1")
}

// RecoveryEnv returns the environment of the recovery process (contracts 2.3).
func (p Paths) RecoveryEnv(cfg *config.Config, incidentID string) []string {
	env := []string{
		"HOME=" + p.BearbotHome,
		"PATH=" + p.ChildPATH(),
		"VIRTUAL_ENV=" + p.PythonVenv,
		"UV_LINK_MODE=copy",
		"PI_SKIP_VERSION_CHECK=1",
		NoJitiCache,
		"PI_CODING_AGENT_DIR=" + p.RecoveryAgentDir,
		"BEARBOT_AGENT_SOCKET=" + p.AgentSock,
		"BEARBOT_INCIDENT_ID=" + incidentID,
	}
	env = append(env, p.gitSafeDirectory()...)
	env = append(env, inherited()...)
	return appendMap(env, cfg.Recovery.Env)
}

// GatewayEnv returns the environment of the gateway (contracts 2.4).
func (p Paths) GatewayEnv() []string {
	env := []string{"PATH=" + p.ChildPATH(), "HOME=" + p.GatewayDataDir, "NODE_ENV=production"}
	return append(env, inherited()...)
}

// ToolEnv returns the environment of helper commands that run as bearbot on the agent's files (uv, python, npm,
// tsc): HOME, PATH, TZ and LANG.
func (p Paths) ToolEnv() []string {
	env := []string{"HOME=" + p.BearbotHome, "PATH=" + p.ChildPATH()}
	return append(env, inherited()...)
}

// RootPATH is the PATH of commands the daemon runs as root (git, maintenance hooks). It must not contain ProfileBin
// or the virtual environment: bearbot can write there.
const RootPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// RootEnv returns the base environment of commands run as root (maintenance hooks, DESIGN 5.8 adds the BEARBOT_*
// event variables): PATH=RootPATH, TZ and LANG.
func (p Paths) RootEnv() []string {
	return append([]string{"PATH=" + RootPATH}, inherited()...)
}
