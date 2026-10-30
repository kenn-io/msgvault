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
	return ClientOptionsWithLimits(storage, version, rate.NewLimiter(rate.Limit(2), 1), 5*time.Minute)
}

// ClientOptionsWithLimits applies a shared budget and ceiling during login.
func ClientOptionsWithLimits(storage session.Storage, version string, limiter *rate.Limiter, maxWait time.Duration) telegram.Options {
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(2), 1)
	}
	return telegram.Options{
		SessionStorage: storage,
		Device:         telegram.DeviceConfig{DeviceModel: "msgvault", AppVersion: version},
		OnTransfer: func(ctx context.Context, _ *telegram.Client, transfer func(context.Context) error) error {
			return transfer(context.WithValue(ctx, transferContextKey{}, true))
		},
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
			return (&Invoker{next: next, limiter: limiter, maxWait: maxWait, allowAuth: true}).Invoke
		})},
	}
}

// ReadClientOptions configures a client that cannot initiate authentication.
func ReadClientOptions(storage session.Storage, version string) telegram.Options {
	return ReadClientOptionsWithLimiter(storage, version, rate.NewLimiter(rate.Limit(2), 1))
}

// ReadClientOptionsWithLimiter shares one rate budget with media connections.
func ReadClientOptionsWithLimiter(storage session.Storage, version string, limiter *rate.Limiter) telegram.Options {
	return ReadClientOptionsWithLimits(storage, version, limiter, 5*time.Minute)
}

// ReadClientOptionsWithLimits shares a request budget and flood-wait ceiling.
func ReadClientOptionsWithLimits(storage session.Storage, version string, limiter *rate.Limiter, maxWait time.Duration) telegram.Options {
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(2), 1)
	}
	opts := ClientOptions(storage, version)
	opts.Middlewares = []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return (&Invoker{next: next, limiter: limiter, maxWait: maxWait}).Invoke
	})}
	return opts
}
