package api

import (
	"context"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"golang.org/x/time/rate"
)

// ClientOptions supplies the session store and read-only request middleware.
func ClientOptions(storage session.Storage, version string) telegram.Options {
	return telegram.Options{
		SessionStorage: storage,
		Device:         telegram.DeviceConfig{DeviceModel: "msgvault", AppVersion: version},
		OnTransfer: func(ctx context.Context, _ *telegram.Client, transfer func(context.Context) error) error {
			return transfer(context.WithValue(ctx, transferContextKey{}, true))
		},
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
			return NewInvoker(next, rate.Limit(2), 1, 5*time.Minute).Invoke
		})},
	}
}

// ReadClientOptions configures a client that cannot initiate authentication.
func ReadClientOptions(storage session.Storage, version string) telegram.Options {
	return ReadClientOptionsWithLimiter(storage, version, rate.NewLimiter(rate.Limit(2), 1))
}

// ReadClientOptionsWithLimiter shares one rate budget with media connections.
func ReadClientOptionsWithLimiter(storage session.Storage, version string, limiter *rate.Limiter) telegram.Options {
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(2), 1)
	}
	opts := ClientOptions(storage, version)
	opts.Middlewares = []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return (&Invoker{next: next, limiter: limiter, maxWait: 5 * time.Minute}).Invoke
	})}
	return opts
}
