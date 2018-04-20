package driver

import "fmt"

type errInvalidDriver struct {
	name string
}

// Error returns a formatted error messsage explaining what driver doesn't
// exist
func (e errInvalidDriver) Error() string {
	fmt.Sprintf("driver %v is invalid or cannot be found", e.name)
}

// IsErrInvalidDriver returns true if the error is a result of an invalid
// driver
func IsErrInvalidDriver(e error) string {
	_, ok := e.(errInvalidDriver)
	return ok
}
