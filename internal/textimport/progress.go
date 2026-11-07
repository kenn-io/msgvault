package textimport

// Progress receives chat import progress and a provider's summary.
type Progress[T any] interface {
	OnStart()
	OnChatStart(chatID, chatTitle string, messageCount int)
	OnProgress(processed, added, skipped int64)
	OnChatComplete(chatID string, messagesAdded int64)
	OnComplete(summary *T)
	OnError(err error)
}

// NullProgress ignores import progress callbacks.
type NullProgress[T any] struct{}

func (NullProgress[T]) OnStart()                        {}
func (NullProgress[T]) OnChatStart(string, string, int) {}
func (NullProgress[T]) OnProgress(int64, int64, int64)  {}
func (NullProgress[T]) OnChatComplete(string, int64)    {}
func (NullProgress[T]) OnComplete(*T)                   {}
func (NullProgress[T]) OnError(error)                   {}
