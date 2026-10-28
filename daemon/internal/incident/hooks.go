package incident

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"bearbot/daemon/internal/layout"
	"bearbot/daemon/internal/proc"
)

// Values of BEARBOT_EVENT for the maintenance hooks.
const (
	EventMaintenanceEnter = "maintenance_enter"
	EventMaintenanceExit  = "maintenance_exit"
	// EventGatewayCrash is used when the gateway exceeded [gateway] crash_limit (DESIGN 5.5 runs on_enter then).
	EventGatewayCrash = "gateway_crash"
)

// HookEvent is the information given to the maintenance hooks as BEARBOT_* environment variables (DESIGN 5.8).
type HookEvent struct {
	Event string
	// IncidentID is empty when there is no incident (rollback error, gateway crash).
	IncidentID string
	Reason     string
	Report     string
}

// RunHooks runs every entry of commands as root with /bin/sh -c, one after another, each bounded by timeout. The
// environment is layout.RootEnv (fixed PATH) plus BEARBOT_EVENT, BEARBOT_INCIDENT_ID, BEARBOT_REASON and
// BEARBOT_REPORT; the working directory is the config directory. A command that cannot start, fails or times out
// is written to the log; the next command still runs.
func RunHooks(ctx context.Context, p layout.Paths, commands []string, timeout time.Duration, ev HookEvent, log *slog.Logger) {
	env := append(p.RootEnv(),
		"BEARBOT_EVENT="+ev.Event,
		"BEARBOT_INCIDENT_ID="+ev.IncidentID,
		"BEARBOT_REASON="+ev.Reason,
		"BEARBOT_REPORT="+ev.Report,
	)
	for _, cmd := range commands {
		log.Info("running maintenance hook", "event", ev.Event, "command", cmd)
		res, err := proc.Run(ctx, proc.RunSpec{Path: "/bin/sh", Args: []string{"-c", cmd}, Env: env, Dir: p.ConfigDir, Timeout: timeout})
		if err != nil {
			log.Error("maintenance hook could not run", "event", ev.Event, "command", cmd, "err", err)
			continue
		}
		out := strings.TrimSpace(string(res.Output))
		if !res.OK() {
			log.Error("maintenance hook failed", "event", ev.Event, "command", cmd, "err", res.Err())
			continue
		}
		log.Info("maintenance hook finished", "event", ev.Event, "command", cmd, "output", out)
	}
}
