package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/time/rate"
)

// Invoker restricts Telegram requests and applies rate and flood-wait limits.
type Invoker struct {
	next      tg.Invoker
	limiter   *rate.Limiter
	maxWait   time.Duration
	allowAuth bool
}

type transferContextKey struct{}

// NewInvoker wraps a transport. A wait exceeding maxWait ends the run.
func NewInvoker(next tg.Invoker, limit rate.Limit, burst int, maxWait time.Duration) *Invoker {
	return &Invoker{next: next, limiter: rate.NewLimiter(limit, burst), maxWait: maxWait, allowAuth: true}
}

// NewReadInvoker excludes authentication methods during archive sync.
func NewReadInvoker(next tg.Invoker, limit rate.Limit, burst int, maxWait time.Duration) *Invoker {
	return &Invoker{next: next, limiter: rate.NewLimiter(limit, burst), maxWait: maxWait}
}

func allowedRequest(input bin.Encoder, depth int, allowAuth bool) bool {
	if input == nil || depth > 16 {
		return false
	}
	value := reflect.ValueOf(input)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return false
	}
	switch request := input.(type) {
	case *tg.InvokeWithTakeoutRequest:
		return request != nil && request.Query != nil && allowedRequest(request.Query, depth+1, false)
	case *tg.InvokeWithMessagesRangeRequest:
		if request == nil || request.Query == nil {
			return false
		}
		switch request.Query.(type) {
		case *tg.MessagesGetDialogsRequest, *tg.MessagesGetHistoryRequest:
			return allowedRequest(request.Query, depth+1, false)
		default:
			return false
		}
	case *tg.AuthSendCodeRequest, *tg.AuthResendCodeRequest, *tg.AuthSignInRequest,
		*tg.AuthCheckPasswordRequest, *tg.AuthExportLoginTokenRequest, *tg.AuthImportLoginTokenRequest,
		*tg.AccountGetPasswordRequest:
		return allowAuth && depth == 0
	case *tg.AccountInitTakeoutSessionRequest:
		return depth == 0
	case *tg.AccountFinishTakeoutSessionRequest:
		return depth <= 1
	case *tg.UsersGetUsersRequest, *tg.UsersGetFullUserRequest,
		*tg.MessagesGetDialogsRequest, *tg.MessagesGetHistoryRequest, *tg.MessagesGetSplitRangesRequest, *tg.MessagesGetMessagesRequest,
		*tg.ChannelsGetFullChannelRequest, *tg.ChannelsGetMessagesRequest,
		*tg.UpdatesGetStateRequest, *tg.UpdatesGetDifferenceRequest, *tg.UpdatesGetChannelDifferenceRequest,
		*tg.UploadGetFileRequest, *tg.UploadGetFileHashesRequest, *tg.UploadGetCDNFileRequest,
		*tg.UploadGetCDNFileHashesRequest, *tg.UploadReuploadCDNFileRequest, *tg.HelpGetCDNConfigRequest:
		return true
	default:
		return false
	}
}

// Invoke refuses unlisted operations, including those nested inside wrappers.
func (i *Invoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	_, transferRequest := input.(*tg.AuthExportAuthorizationRequest)
	transferAllowed := transferRequest && !reflect.ValueOf(input).IsNil() && ctx.Value(transferContextKey{}) == true
	if !transferAllowed && !allowedRequest(input, 0, i.allowAuth) {
		return errors.New("Telegram request is not permitted")
	}
	for {
		if err := i.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("wait for Telegram rate limit: %w", err)
		}
		err := i.next.Invoke(ctx, input, output)
		for _, kind := range tgerr.FloodWaitErrors {
			if rpc, ok := tgerr.AsType(err, kind); ok && (rpc.Argument < 0 || int64(rpc.Argument) > math.MaxInt64/int64(time.Second)) {
				return fmt.Errorf("invalid Telegram flood wait: %w", err)
			}
		}
		delay, flood := tgerr.AsFloodWait(err)
		if !flood {
			return err
		}
		if delay > i.maxWait {
			return fmt.Errorf("Telegram flood wait %s exceeds limit %s: %w", delay, i.maxWait, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
