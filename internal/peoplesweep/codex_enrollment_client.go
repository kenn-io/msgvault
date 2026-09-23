package peoplesweep

import "context"

// CodexEnrollment is the model-less daemon enrollment surface. A draft auth
// home belongs to one caller; its owner serializes login, model listing, and
// later inference for that home.
type CodexEnrollment interface {
	StartDeviceLogin(ctx context.Context, present func(DeviceLogin) error) error
	ListModels(ctx context.Context) ([]CodexModel, error)
}
