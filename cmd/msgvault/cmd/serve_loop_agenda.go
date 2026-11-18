package cmd

import (
	"context"
	"net"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func registerLoopAgendaJob(sched *scheduler.Scheduler, cfg config.TaskIntegrationConfig, agenda *api.LoopAgenda) error {
	if !cfg.Enabled || cfg.AutoAgenda != nil && !*cfg.AutoAgenda {
		return nil
	}
	return sched.AddJob(scheduler.Job{Name: "loops-agenda", Schedule: "0 * * * *", Run: func(ctx context.Context) error { return agenda.Run(ctx, time.Now()) }})
}

// loopAgendaOrigin is the address issues filed without a browser request link
// to until a signed-in browser shows which address it uses. A server listening
// on every interface has no address of its own a browser is known to reach,
// so those issues carry no link.
func loopAgendaOrigin(listener net.Addr) string {
	addr, ok := listener.(*net.TCPAddr)
	if !ok || addr.IP.IsUnspecified() {
		return ""
	}
	return "http://" + addr.String()
}
