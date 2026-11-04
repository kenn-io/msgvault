package query

import (
	"context"
	"maps"
	"net/mail"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/senderclass"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// SenderClassResolver reads exact archive metadata. Implementations must honor
// source scope on every lookup, including contact reachability and prior sends.
type SenderClassResolver interface {
	MessageSenderIdentities(ctx context.Context, messageIDs, allowedSourceIDs []int64) (map[int64]store.MessageSenderIdentity, error)
	SenderRelationships(ctx context.Context, addresses []string, allowedSourceIDs []int64) (map[string]store.SenderRelationshipEvidence, error)
}

type senderClassPolicy struct{ resolver SenderClassResolver }

// ConfigureSenderClass installs the archive's relationship reader before an
// engine is published. It is forwarded without hiding optional capabilities.
func ConfigureSenderClass(engine Engine, resolver SenderClassResolver) {
	if setter, ok := engine.(interface {
		SetSenderClassResolver(resolver SenderClassResolver)
	}); ok {
		setter.SetSenderClassResolver(resolver)
	}
}

func (e *SQLiteEngine) SetSenderClassResolver(resolver SenderClassResolver) {
	e.senderClassPolicy.Store(&senderClassPolicy{resolver: resolver})
}
func (e *pgEngine) SetSenderClassResolver(resolver SenderClassResolver) {
	ConfigureSenderClass(e.Engine, resolver)
}
func (e *DuckDBEngine) SetSenderClassResolver(resolver SenderClassResolver) {
	if e.sqliteEngine != nil {
		e.sqliteEngine.SetSenderClassResolver(resolver)
	}
}

// SenderClassInput binds a selected row and its authenticated receipt to the
// exact From identity displayed by that row, including stale cached pages.
type SenderClassInput struct {
	ID         int64
	From       []Address
	SenderAuth *senderauth.Verdict
}

type SenderClassReader interface {
	ReadSenderClasses(ctx context.Context, messages []SenderClassInput) map[int64]senderclass.Readout
}

const senderClassIdentityBatchSize = 500

// ReadSenderClasses fails closed when an engine cannot read the required
// metadata. Missing evidence never becomes a definitive none relationship.
func ReadSenderClasses(ctx context.Context, engine Engine, messages []SenderClassInput) map[int64]senderclass.Readout {
	if reader, ok := engine.(SenderClassReader); ok {
		return reader.ReadSenderClasses(ctx, messages)
	}
	return unavailableSenderClasses(messages)
}

func unavailableSenderClasses(messages []SenderClassInput) map[int64]senderclass.Readout {
	out := make(map[int64]senderclass.Readout, len(messages))
	for _, message := range messages {
		out[message.ID] = senderclass.UnavailableReadout()
	}
	return out
}

func (e *SQLiteEngine) ReadSenderClasses(ctx context.Context, messages []SenderClassInput) map[int64]senderclass.Readout {
	out := unavailableSenderClasses(messages)
	policy := e.senderClassPolicy.Load()
	if policy == nil || policy.resolver == nil || len(messages) == 0 {
		return out
	}
	for start := 0; start < len(messages); start += senderClassIdentityBatchSize {
		end := min(start+senderClassIdentityBatchSize, len(messages))
		maps.Copy(out, e.readSenderClassBatch(ctx, messages[start:end], policy))
	}
	return out
}

func (e *SQLiteEngine) readSenderClassBatch(ctx context.Context, messages []SenderClassInput, policy *senderClassPolicy) map[int64]senderclass.Readout {
	out := unavailableSenderClasses(messages)
	ids := make([]int64, len(messages))
	for i, message := range messages {
		ids[i] = message.ID
	}
	scope := sourcescope.IDs(ctx)
	identities, err := policy.resolver.MessageSenderIdentities(ctx, ids, scope)
	if err != nil {
		return out
	}
	keys := make([]string, 0, len(messages))
	for _, message := range messages {
		identity, exists := identities[message.ID]
		if !exists {
			continue
		}
		if identity.SourceIsFromMe {
			out[message.ID] = senderclass.Readout{SenderClass: senderclass.Self, SenderClassBasis: senderclass.SourceOwned, SenderRelationship: senderclass.RelationshipNone}
		} else if senderClassAddressMatches(identity.Address, message.From) {
			keys = append(keys, identity.Address)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	relationships := make(map[string]store.SenderRelationshipEvidence, len(keys))
	for start := 0; start < len(keys); start += store.MaxSenderRelationshipAddresses {
		batch := keys[start:min(start+store.MaxSenderRelationshipAddresses, len(keys))]
		found, err := policy.resolver.SenderRelationships(ctx, batch, scope)
		if err != nil {
			// Preserve only independently established native self evidence.
			return out
		}
		maps.Copy(relationships, found)
	}
	for _, message := range messages {
		identity, exists := identities[message.ID]
		if !exists || identity.SourceIsFromMe || !senderClassAddressMatches(identity.Address, message.From) {
			continue
		}
		relationship, complete := relationships[identity.Address]
		if !complete {
			continue
		}
		relation := senderclass.Relationship(relationship.Relationship)
		if relation != senderclass.RelationshipNone && relation != senderclass.RelationshipKnown && relation != senderclass.RelationshipContact {
			continue
		}
		result := senderclass.Classify(senderclass.Evidence{Auth: senderClassAuth(message.SenderAuth), Relationship: relation})
		out[message.ID] = senderclass.Readout{SenderClass: result.Class, SenderClassBasis: result.Basis, SenderRelationship: relation}
	}
	return out
}

func senderClassAddressMatches(address string, from []Address) bool {
	if address == "" || len(from) != 1 {
		return false
	}
	parsed, err := mail.ParseAddress(from[0].Email)
	return err == nil && strings.EqualFold(address, parsed.Address)
}

func senderClassAuth(verdict *senderauth.Verdict) senderclass.AuthState {
	if verdict == nil || verdict.Kind != "email" {
		return senderclass.AuthNone
	}
	if verdict.Verified && verdict.DMARC == senderauth.Pass {
		return senderclass.AuthPass
	}
	if verdict.DMARC == senderauth.Fail || verdict.DKIM == senderauth.Fail {
		return senderclass.AuthFail
	}
	return senderclass.AuthNone
}

func (e *pgEngine) ReadSenderClasses(ctx context.Context, messages []SenderClassInput) map[int64]senderclass.Readout {
	return ReadSenderClasses(ctx, e.Engine, messages)
}
func (e *DuckDBEngine) ReadSenderClasses(ctx context.Context, messages []SenderClassInput) map[int64]senderclass.Readout {
	if e.sqliteEngine != nil {
		return e.sqliteEngine.ReadSenderClasses(ctx, messages)
	}
	return unavailableSenderClasses(messages)
}

func fillSummarySenderClasses(ctx context.Context, engine Engine, messages []MessageSummary) {
	inputs := make([]SenderClassInput, len(messages))
	for i, message := range messages {
		inputs[i] = SenderClassInput{ID: message.ID, From: []Address{{Email: message.FromEmail}}, SenderAuth: message.SenderAuth}
	}
	results := ReadSenderClasses(ctx, engine, inputs)
	for i := range messages {
		messages[i].Readout = results[messages[i].ID]
	}
}
