package api

import (
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
		Middlewares: []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
			return NewInvoker(next, rate.Limit(2), 1, 5*time.Minute).Invoke
		})},
	}
}

// ReadClientOptions configures a client that cannot initiate authentication.
func ReadClientOptions(storage session.Storage, version string) telegram.Options {
	opts := ClientOptions(storage, version)
	opts.Middlewares = []telegram.Middleware{telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return NewReadInvoker(next, rate.Limit(2), 1, 5*time.Minute).Invoke
	})}
	return opts
}
