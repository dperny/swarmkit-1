package ipam

import (
	"fmt"
)

// ErrNetworkNotAllocated is an error type returned when some attachment or
// VIP relies on a network that is not yet allocated. If this error occurs
// during allocation, then allocation should be retried after the network is
// allocated. However, if it occurs during restore, it indicates that the state
// of the raft store is inconsistent
type ErrNetworkNotAllocated struct {
	nwid string
}

// Error returns a formatted error string explaining which network is not
// allocated
func (e ErrNetworkNotAllocated) Error() string {
	return fmt.Sprintf("network %v is not allocated", e.nwid)
}

// IsErrNetworkNotAllocated returns true if the type of the error is
// ErrNetworkNotAllocated
func IsErrNetworkNotAllocated(e error) bool {
	_, ok := e.(ErrNetworkNotAllocated)
	return ok
}

// ErrInvalidIPAM is an error type returned if a given IPAM driver is invalid.
type ErrInvalidIPAM struct {
	ipam string
}

// Error returns a formatted error string explaining which IPAM driver is not
// valid
func (e ErrInvalidIPAM) Error() string {
	return fmt.Sprintf("ipam driver %v is not valid", e.ipam)
}

// IsErrInvalidIPAM returns true if the type of the error is ErrInvalidIPAM.
func IsErrInvalidIPAM(e error) bool {
	_, ok := e.(ErrInvalidIPAM)
	return ok
}

// ErrInvalidAddress is an error type indicating that a requested address's
// string form is not a valid address and it cannot be parsed.
type ErrInvalidAddress struct {
	address string
}

// Error returns a formatted error message explaining which address is invalid
func (e ErrInvalidAddress) Error() string {
	return fmt.Sprintf("address %v is not a valid IP address", e.address)
}

// IsErrInvalidAddress returns true if the type of the error is
// ErrInvalidAddress.
func IsErrInvalidAddress(e error) bool {
	_, ok := e.(ErrInvalidAddress)
	return ok
}
