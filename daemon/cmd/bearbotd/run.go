package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"bearbot/daemon/internal/agent"
	"bearbot/daemon/internal/api"
	"bearbot/daemon/internal/config"
	"bearbot/daemon/internal/gateway"
	"bearbot/daemon/internal/incident"
	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
	"bearbot/daemon/internal/recovery"
	"bearbot/daemon/internal/snapshot"
	"bearbot/daemon/internal/state"
)

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// runDaemon is `bearbotd run`: the startup sequence of DESIGN 5.2 and the shutdown on SIGTERM or SIGINT.
func runDaemon(g *globals, configPath string) error {
	unix.Umask(0o002)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	paths, err := g.paths()
	if err != nil {
		return err
	}
	if configPath == "" {
		configPath = paths.DefaultConfigFile
	}
	// Step 1.
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	users, err := layout.LookupUsers(g.rootless)
	if err != nil {
		return err
	}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	// Step 2.
	initRes, err := layout.Init(baseCtx, paths, users)
	if err != nil {
		return err
	}
	daemonLog := proc.NewLogWriter(paths.DaemonLog, 2000)
	defer daemonLog.Close()
	log := slog.New(slog.NewTextHandler(daemonLog, &slog.HandlerOptions{Level: logLevel(cfg.Daemon.LogLevel)}))
	log.Info("bearbotd starting", "pid", os.Getpid(), "root", paths.Root, "rootless", g.rootless, "config", configPath,
		"createdDirs", initRes.CreatedDirs, "wroteSettings", initRes.WroteSettings, "wroteTemplates", initRes.WroteTemplates,
		"createdVenv", initRes.CreatedVenv, "python", initRes.PythonVersion)

	err = serve(baseCtx, paths, users, cfg, log, daemonLog, signals)
	if err != nil {
		log.Error("bearbotd exits because of an error", "err", err)
		return err
	}
	log.Info("bearbotd stopped")
	return nil
}

func serve(baseCtx context.Context, paths layout.Paths, users *layout.Users, cfg *config.Config, log *slog.Logger, daemonLog *proc.LogWriter, signals chan os.Signal) error {
	// Step 3.
	repo, err := snapshot.Open(paths, users)
	if err != nil {
		return err
	}
	created, err := repo.Init(baseCtx)
	if err != nil {
		return err
	}
	log.Info("snapshot repository ready", "created", created)

	// Step 4.
	st, exists, err := state.Load(paths.StateFile)
	if err != nil {
		return err
	}
	if !exists {
		if err := state.Save(paths.StateFile, st); err != nil {
			return err
		}
	}
	// The recovery credentials exist only while a repair runs (DESIGN 15); a profile left by a daemon that stopped
	// during a repair goes before anything starts (the run directory is not always a new tmpfs).
	if err := recovery.RemoveProfile(paths); err != nil {
		return err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("找不到 node 命令：%w", err)
	}

	// Step 6 is decided before the supervisors exist: the console goes into gateway.json and StatusInfo.
	capable, kind, err := gateway.ConsoleInput(os.Stdin)
	if err != nil {
		return err
	}
	console := cfg.Platforms.Console.Enabled && capable
	log.Info("console decision", "enabled", console, "configEnabled", cfg.Platforms.Console.Enabled, "stdin", kind)

	gw, err := gateway.New(gateway.Options{
		Paths:   paths,
		Users:   users,
		Config:  cfg,
		Logger:  log.With("component", "gateway"),
		Console: console,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		OnCrashLimit: func(reason, report string) {
			incident.RunHooks(baseCtx, paths, cfg.Maintenance.OnEnter, cfg.Maintenance.HookTimeout,
				incident.HookEvent{Event: incident.EventGatewayCrash, Reason: reason, Report: report}, log)
		},
	})
	if err != nil {
		return err
	}
	sup := agent.New(agent.Options{
		Paths:   paths,
		Users:   users,
		Config:  cfg,
		Repo:    repo,
		Gateway: gw,
		Logger:  log.With("component", "agent"),
		State:   st,
		Console: console,
		Node:    node,
	})
	defer func() {
		if err := sup.Close(); err != nil {
			log.Error("close agent logs", "err", err)
		}
	}()
	// Step 4: the unfinished member turn is compared before anything starts.
	if err := sup.Prepare(baseCtx, created); err != nil {
		return err
	}
	server := &api.Server{Sup: sup, Gateway: gw, Paths: paths, Config: cfg, Log: log.With("component", "api"), DaemonLog: daemonLog, Ctx: baseCtx}
	gw.SetHandler(server)
	gw.SetStateSource(sup.WireState)

	// Step 5.
	agentLn, err := api.ListenUnix(paths.AgentSock, 0o660, users.Root.UID, users.BearbotGroup, users)
	if err != nil {
		return err
	}
	adminLn, err := api.ListenUnix(paths.AdminSock, 0o600, users.Root.UID, users.Root.GID, users)
	if err != nil {
		agentLn.Close()
		return err
	}
	agentSrv := api.NewHTTPServer(server.AgentHandler(), log)
	adminSrv := api.NewHTTPServer(server.AdminHandler(), log)
	serveErr := make(chan error, 2)
	go func() { serveErr <- serveHTTP(agentSrv, agentLn) }()
	go func() { serveErr <- serveHTTP(adminSrv, adminLn) }()
	defer func() {
		agentSrv.Close()
		adminSrv.Close()
	}()

	// Step 7.
	if err := gateway.WriteFile(paths, users, gateway.BuildFile(paths, users, cfg, console)); err != nil {
		return err
	}
	gwDone := make(chan struct{})
	go func() {
		gw.Run(baseCtx)
		close(gwDone)
	}()
	stopGateway := func() {
		if err := gw.Stop(cfg.Daemon.TermGrace); err != nil {
			log.Error("stop gateway", "err", err)
		}
	}
	connected := make(chan error, 1)
	waitCtx, cancelWait := context.WithCancel(baseCtx)
	defer cancelWait()
	go func() { connected <- gw.WaitConnected(waitCtx) }()
	select {
	case <-connected:
		log.Info("gateway ready")
	case sig := <-signals:
		log.Info("signal before the gateway was ready, stopping", "signal", sig.String())
		stopGateway()
		return nil
	case err := <-serveErr:
		stopGateway()
		return err
	}

	// Step 8.
	if !cfg.Platforms.Console.Enabled && capable {
		go func() {
			if err := gateway.PromptUnavailable(os.Stdin, os.Stdout); err != nil {
				log.Error("console notice on stdin", "err", err)
			}
		}()
	}

	// Step 9.
	lifeCtx, cancelLife := context.WithCancel(baseCtx)
	defer cancelLife()
	supDone := make(chan error, 1)
	go func() { supDone <- sup.Run(lifeCtx) }()

	var runErr error
	select {
	case sig := <-signals:
		log.Info("shutting down", "signal", sig.String())
	case runErr = <-supDone:
		supDone = nil
		if runErr == nil {
			runErr = errors.New("agent supervisor stopped by itself")
		}
	case runErr = <-serveErr:
	}

	// Shutdown (DESIGN 5.2): stop receiving, let the turn end, stop the agent, stop the gateway.
	if err := gw.Drain(baseCtx); err != nil {
		log.Error("drain gateway", "err", err)
	}
	cancelLife()
	if supDone != nil {
		if err := <-supDone; err != nil && runErr == nil {
			runErr = err
		}
	}
	stopGateway()
	<-gwDone
	return runErr
}

func serveHTTP(srv *http.Server, l net.Listener) error {
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务（%s）失败：%w", l.Addr(), err)
	}
	return nil
}
