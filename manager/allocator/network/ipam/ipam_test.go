package ipam_test

import (
	"net"

	"github.com/docker/libnetwork/discoverapi"
	"github.com/docker/libnetwork/ipamapi"

	"github.com/docker/swarmkit/api"

	. "github.com/docker/swarmkit/manager/allocator/network/ipam"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type ipamAndCaps struct {
	ipam ipamapi.Ipam
	caps *ipamapi.Capability
}

type mockDrvRegistry struct {
	ipams map[string]ipamAndCaps
	// injectIpam lets use call some function before we actually get an IPAM
	beforeIpam func()
}

func (m *mockDrvRegistry) IPAM(name string) (ipamapi.Ipam, *ipamapi.Capability) {
	m.beforeIpam()
	i, ok := m.ipams[name]
	if !ok {
		return nil, nil
	}
	return i.ipam, i.caps
}

// mockIPAM is an object filling the ipamapi.Ipam interface, which lets us
// inject whatever code we want instead of a real IPAM driver. We only care
// about or use 2 different IPAM functions in the ipam module, and everything
// else will panic (to abort the test and tell us that an unexpected method was
// called
type mockIpam struct {
	getDefaultAddressSpacesFunc func() (string, string, error)
	requestPoolFunc             func(string, string, string, map[string]string, bool) (string, *net.IPNet, map[string]string, error)
	releasePoolFunc             func(string) error
	requestAddressFunc          func(string, net.IP, map[string]string) (*net.IPNet, map[string]string, error)
	releaseAddressFunc          func(string, net.IP) error
	isBuiltIn                   bool
}

// we need to fill the discoverapi.Discoverer interface to fill the Ipam
// interface

// DiscoverNew panics if called
func (m *mockIpam) DiscoverNew(_ discoverapi.DiscoveryType, _ interface{}) error {
	panic("DiscoverNew not implemented")
}

// DiscoverDelete panics if called
func (m *mockIpam) DiscoverDelete(_ discoverapi.DiscoveryType, _ interface{}) error {
	panic("DiscoverDelete not implemented")
}

func (m *mockIpam) GetDefaultAddressSpaces() (string, string, error) {
	return m.getDefaultAddressSpacesFunc()
}

func (m *mockIpam) RequestPool(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
	return m.requestPoolFunc(addressSpace, pool, subpool, options, v6)
}

func (m *mockIpam) ReleasePool(poolID string) error {
	return m.releasePoolFunc(poolID)
}

func (m *mockIpam) RequestAddress(poolID string, ip net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	return m.requestAddressFunc(poolID, ip, opts)
}

func (m *mockIpam) ReleaseAddress(poolID string, ip net.IP) error {
	return m.releaseAddressFunc(poolID, ip)
}

func (m *mockIpam) IsBuiltIn() bool {
	return m.isBuiltIn
}

var _ = Describe("ipam.Allocator", func() {
	var (
		reg *mockDrvRegistry
		a   *Allocator
	)
	BeforeEach(func() {
		reg = &mockDrvRegistry{
			ipams: make(map[string]ipamAndCaps),
		}
		a = NewAllocator(reg)
	})
	Describe("Restoring pre-existing allocations", func() {
		Context("When nothing is allocated", func() {
			var (
				err       error
				wasCalled bool
			)
			BeforeEach(func() {
				reg.beforeIpam = func() {
					wasCalled = true
				}
				err = a.Restore(nil, nil, nil)
			})
			It("should succeed", func() {
				Expect(err).ToNot(HaveOccurred())
			})
			It("should not try to get an IPAM driver", func() {
				Expect(wasCalled).To(BeFalse())
			})
		})
	})
	Context("When passed unallocated objects", func() {
		var (
			err       error
			wasCalled bool
		)
		BeforeEach(func() {
			reg.beforeIpam = func() { wasCalled = true }
			networks := []*api.Network{
				{
					ID: "net1",
					Driver: *api.Driver{
						Name:    "overlay",
						Options: map[string]string{},
					},
					Spec: api.NetworkSpec{
						DriverConfig: *api.Driver{
							Name:    "overlay",
							Options: map[string]string{},
						},
						IPAM: *api.IPAMOptions{
							Driver: *api.Driver{
								Name: "ipamdriver",
							},
							Configs: []*api.IPAMConfig{
								{
									Family:  api.IPAMConfig_IPV4,
									Subnet:  "192.168.0.1/24",
									Range:   "192.168.0.1/24",
									Gateway: "192.168.0.1",
								},
							},
						},
					},
				},
			}
			endpoints := []*api.Endpoint{}
		})
	})
})
