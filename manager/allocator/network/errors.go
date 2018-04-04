package network

// ErrorIsRetryable is an interface that errors in this package may fulfill. If
// an error is of type ErrorIsRetryable, it indicates that whatever failure
// occured isn't due to any misconfiguration, but its due to some state failure
// that may be retriable.
type ErrorRetryable interface {
	Retryable()
}

// IsErrorRetryable returns true if the provided error indicates a situation
// that can be retried
func IsErrorRetryable(e error) bool {
	_, ok := e.(ErrorRetryable)
	return ok
}

// ErrResourceExhausted indicates that the error is caused by some resource
// type, like IP addresses or ports, being exhausted. The caller should try
// again after some services have been updated or deleted
type ErrResourceExhausted struct {
	// cause is the cause of this error, which should include a descriptive
	// message for the user incidating what resource is exhausted
	cause error
}

// IsErrResourceExhausted returns true if the error is a ErrResourceExhausted
func IsErrResourceExhausted(e error) bool {
	_, ok := e.(ErrResourceExhausted)
	return ok
}

// Error returns the cause of ErrResourceExhausted
func (e ErrResourceExhausted) Error() string {
	return e.cause.Error()
}

// Retryable fulfills the ErrorRetryable interface and indicates that whatever
// caused ErrResourceExhausted can be retried later
func (e ErrResourceExhausted) Retryable() {}

// ErrResourceInUse indicates that some requested resource is in use. The
// resource may be freed and become available later
type ErrResourceInUse struct {
	cause error
}

func IsErrResourceInUse(e error) bool {
	_, ok := e.(ErrResourceInUse)
	return ok
}

// Error returns the error message of the cause of this error
func (e ErrResourceInUse) Error() string {
	return e.cause.Error()
}

// Retryable fulfills the ErrorRetryable interface and indicates that whatever
// caused ErrResourceInUse can be retried later.
func (e ErrResourceInUse) Retryable() {}

// ErrInvalidSpec indicates that some element of the spec of the object
// requested for allocation is invalid and the allocation can never succeed.
// The caller should not retry allocation with this object.
type ErrInvalidSpec struct {
	cause error
}

// IsErrInvalidSpec returns true if the error is ErrInvalidSpec
func IsErrInvalidSpec(e error) bool {
	_, ok := e.(ErrInvalidSpec)
	return ok
}

// Error returns the cause of this invalid spec error.
func (e ErrInvalidSpec) Error() string {
	return e.cause.Error()
}
