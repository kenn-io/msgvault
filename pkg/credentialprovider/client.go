package credentialprovider

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Exchange runs operator-configured argv directly, without a shell. The helper
// inherits the caller's environment and is trusted same-user code, not a sandbox.
// Stderr is discarded; stdout is bounded and discarded on any process failure.
// A returned denial is a valid exchange. Errors never imply remote rollback:
// retain the request for exact recovery instead of allocating another operation.
func Exchange(parent context.Context, argv []string, request Request) (Response, error) {
	if !validRequest(request) || len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return Response{}, ErrInvalidRequest
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, '\x00') {
			return Response{}, ErrInvalidRequest
		}
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > MaxDocumentBytes {
		return Response{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(parent, AttemptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- trusted local executable and argv, never supplied by a remote response.
	cmd.Stdin = bytes.NewReader(data)
	// Bound inherited-pipe cleanup if the helper exits while a descendant keeps
	// stdout open. runHelper also terminates its process group or Windows job.
	cmd.WaitDelay = time.Second
	out := &boundedOutput{cancel: cancel}
	cmd.Stdout = out
	err = runHelper(cmd)
	if out.overflow {
		return Response{}, ErrInvalidResponse
	}
	if ctx.Err() != nil {
		return Response{}, errors.Join(ErrProviderFailed, ctx.Err())
	}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 2 {
			return Response{}, ErrInvalidRequest
		}
		return Response{}, ErrProviderFailed
	}
	return decodeResponse(out.buffer.Bytes(), request)
}

// Do not embed bytes.Buffer: its ReadFrom would bypass the limit in io.Copy.
// Only exec's stdout goroutine writes; runHelper waits before we read it.
type boundedOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (o *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > MaxDocumentBytes-o.buffer.Len() {
		o.overflow = true
		o.cancel()
		return 0, ErrInvalidResponse
	}
	n, _ := o.buffer.Write(data) // bytes.Buffer.Write never returns an error.
	return n, nil
}
