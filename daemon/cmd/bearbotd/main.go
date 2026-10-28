// Command bearbotd is the container entry point of bearbot (DESIGN 5): `run` starts the daemon; the management
// subcommands talk to the running daemon over admin.sock; `login` and `python rebuild` run directly.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"bearbot/daemon/internal/cli"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/snapshot"
)

type globals struct {
	root     string
	rootless bool
}

func (g *globals) paths() (layout.Paths, error) { return layout.NewPaths(g.root) }

func main() {
	var g globals
	root := &cobra.Command{
		Use:           "bearbotd",
		Short:         "bearbot 的 daemon 与管理命令",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.root, "root", "/", "所有路径的根目录前缀（本地测试时使用）")
	root.PersistentFlags().BoolVar(&g.rootless, "rootless", false, "以普通用户运行：子进程不切换用户，不修改属主（只用于本地测试）")

	var runConfig string
	run := &cobra.Command{
		Use:   "run",
		Short: "启动 daemon（容器入口）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemon(&g, runConfig)
		},
	}
	run.Flags().StringVar(&runConfig, "config", "", "bearbot.toml 的路径（默认为 <root>/bearbot/config/bearbot.toml）")

	var loginConfig string
	login := &cobra.Command{
		Use:   "login",
		Short: "以 bearbot 身份运行交互式 pi -ne，在其中执行 /login（需要 docker exec -it）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(&g, loginConfig)
		},
	}
	login.Flags().StringVar(&loginConfig, "config", "", "bearbot.toml 的路径（默认为 <root>/bearbot/config/bearbot.toml）")

	python := &cobra.Command{Use: "python", Short: "Python 虚拟环境", Args: cobra.NoArgs}
	python.AddCommand(&cobra.Command{
		Use:   "rebuild",
		Short: "重建 Python 虚拟环境并装回旧环境中的包（daemon 没有运行时用 docker run --rm 执行）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := g.paths()
			if err != nil {
				return err
			}
			users, err := layout.LookupUsers(g.rootless)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return layout.PythonRebuild(ctx, paths, users, os.Stdout)
		},
	})

	// The work tree side of the snapshots, run by the daemon as bearbot (snapshot.HelperCommand); not for users.
	helper := &cobra.Command{
		Use:                snapshot.HelperCommand,
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return snapshot.RunHelper(args)
		},
	}

	root.AddCommand(run, login, python, helper)
	root.AddCommand(cli.Commands(func() (*cli.Client, error) {
		paths, err := g.paths()
		if err != nil {
			return nil, err
		}
		return &cli.Client{Socket: paths.AdminSock, Out: os.Stdout}, nil
	})...)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bearbotd："+err.Error())
		os.Exit(1)
	}
}

// runLogin runs `<node> <PiBin> -ne` attached to the caller's terminal as bearbot, cwd <BearbotHome>, with the agent
// environment (contracts 2.5). node is looked up in this process's PATH: the child PATH begins with directories
// bearbot can write, and the administrator types credentials on this terminal.
func runLogin(g *globals, configPath string) error {
	paths, err := g.paths()
	if err != nil {
		return err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("找不到 node 命令：%w", err)
	}
	if configPath == "" {
		configPath = paths.DefaultConfigFile
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	users, err := layout.LookupUsers(g.rootless)
	if err != nil {
		return err
	}
	// pi shares the terminal: Ctrl-C and Ctrl-\ are meant for pi, so bearbotd waits for it instead of exiting, also
	// for keys pressed while it returns. The signals are caught (not ignored), so pi keeps the default handling.
	terminalSignals := make(chan os.Signal, 8)
	signal.Notify(terminalSignals, syscall.SIGINT, syscall.SIGQUIT)
	status, err := proc.RunAttached(proc.AttachedSpec{
		Path:       node,
		Args:       append([]string{paths.PiBin}, paths.LoginArgs()...),
		Env:        paths.AgentEnv(cfg),
		Dir:        paths.BearbotHome,
		Credential: users.Credential(users.Bearbot),
	})
	if err != nil {
		return err
	}
	if !status.Success() {
		return fmt.Errorf("pi 以%s结束", status)
	}
	return nil
}
