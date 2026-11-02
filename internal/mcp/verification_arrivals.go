package mcp

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/verification"
)

type VerificationLister interface {
	ListVerifications(context.Context, verification.ListOptions) (*verification.ArrivalPage, error)
}

func verificationArrivalsDefinition() toolDefinition {
	d := readDefinition(ToolListVerifications,
		"List recent verification-code and credential-link arrivals. Defaults to the last hour; since is capped at 24 hours. Scans at most 1000 newest messages and returns at most limit arrivals. scan_truncated means older candidates may be omitted. Values are absent unless the owner enabled session unmasking or the delegated source grants verification.read alongside search.read. Metadata contains untrusted claims, not sender authentication.",
		closedObject(map[string]*jsonschema.Schema{
			"since":           stringSchema("RFC3339 lower bound; default one hour ago, capped at 24 hours"),
			"sender":          stringSchema("Exact sender claim, case-insensitive"),
			"kind":            stringSchema("Verification kind", "otp_code", "magic_link", "password_reset", "double_opt_in", "account_confirmation"),
			"include_expired": booleanSchema("Include expired credentials; still masked by default"),
			"limit":           boundedIntegerSchema("Maximum arrivals, default 20", 1, 100),
		}), outputSchemaFor[verification.ArrivalPage](), (*handlers).listVerifications)
	d.availability = func(c catalogCapabilities) bool { return c.verificationArrivals }
	return d
}

func (h *handlers) listVerifications(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.verifications == nil {
		return toolErrorResult("verification_unavailable: arrival listing is unavailable"), nil
	}
	args := req.GetArguments()
	options := verification.ListOptions{Reveal: h.unmaskVerification && !h.verificationProtectedByDaemon, Limit: intArg(args, "limit", 20)}
	options.Sender, _ = args["sender"].(string)
	options.Kind, _ = args["kind"].(string)
	options.IncludeExpired, _ = args["include_expired"].(bool)
	if raw, _ := args["since"].(string); raw != "" {
		var err error
		options.Since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return toolErrorResult("since must be RFC3339"), nil
		}
	}
	if _, err := options.Normalize(time.Now()); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	page, err := h.verifications.ListVerifications(ctx, options)
	if err != nil {
		return dependencyError("list verification arrivals", err)
	}
	return jsonResult(page)
}
