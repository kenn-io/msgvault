package daemonclient

import "go.kenn.io/msgvault/internal/store"

func senderCeilingFromGenerated[C, B ~string](ceiling *C, basis *B) store.SenderCeilingReadout {
	if ceiling == nil || basis == nil {
		return store.SenderCeilingReadout{}
	}
	result := store.SenderCeiling(store.SenderClassCeiling(*ceiling))
	if result.SenderClassCeilingBasis != store.SenderCeilingBasis(*basis) {
		return store.SenderCeilingReadout{}
	}
	return result
}
