package notify

// PartialError lets a channel report the events of a message that reached
// every recipient even though the send as a whole failed.
type PartialError struct {
	Done []int64
	Err  error
}

func (e *PartialError) Error() string {
	return e.Err.Error()
}

func (e *PartialError) Unwrap() error {
	return e.Err
}
