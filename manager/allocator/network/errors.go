package network

import (
	"fmt"
)

// errDependencyNotAllocated is the error type returned if we're trying to
// allocate a service or task that depends on a network or service not
// allocated yet.
type errDependencyNotAllocated struct {
	objectType string
	id         string
}

// Error returns the error message
func (e errDependencyNotAllocated) Error() string {
	return fmt.Sprintf("%v %v depended on by object is not allocated", e.objectType, e.id)
}

// IsErrDependencyNotAllocated returns true if the type of the error is
// errDependencyNotAllocated
func IsErrDependencyNotAllocated(e error) bool {
	_, ok := e.(errDependencyNotAllocated)
	return ok
}
