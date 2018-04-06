package ipam_test

import (
	"fmt"
	"net"
	"strconv"

	"github.com/docker/libnetwork/discoverapi"
	"github.com/docker/libnetwork/ipamapi"
	"github.com/docker/libnetwork/netlabel"

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

// addressRestorerMockIpam is a mockIpam object that does not allocate new
// addresses or pools, only restores ones that have already been assigned. it
// records which addresses and pools it has been called with
type addressRestorerMockIpam struct {
	*mockIpam
	addresses []string
	pools     []struct {
		subnet  string
		iprange string
	}
}

// RequestPool will the args we don't care about elided
func (m *addressRestorerMockIpam) RequestPool(_ string, subnet string, iprange string, options map[string]string, _ bool) (string, *net.IPNet, map[string]string, error) {
	m.pools = append(m.pools, struct{ subnet, iprange string }{subnet, iprange})
	// record the subnet
	ip := net.ParseIP(subnet)
	return strconv.Itoa(len(m.pools)), &net.IPNet{ip, ip.DefaultMask()}, nil, nil
}

func (m *addressRestorerMockIpam) RequestAddress(_ string, ip net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	ips := ip.String()
	if r, ok := opts[ipamapi.RequestAddressType]; ok && r == netlabel.Gateway {
		ips = ips + "gateway"
	}
	m.addresses = append(m.addresses, ips)
	return &net.IPNet{ip, ip.DefaultMask()}, nil, nil
}

var _ = Describe("ipam.Allocator", func() {
	var (
		reg *mockDrvRegistry
		a   *Allocator
	)
	BeforeEach(func() {
		reg = &mockDrvRegistry{
			ipams: make(map[string]ipamAndCaps),
			// add a noop before function so we don't try calling a nil
			beforeIpam: func() {},
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
		Context("When passed unallocated objects", func() {
			var (
				err                    error
				wasCalled              bool
				network, netCopy       *api.Network
				endpoint, endCopy      *api.Endpoint
				attachment, attachCopy *api.NetworkAttachment
			)
			BeforeEach(func() {
				reg.beforeIpam = func() { wasCalled = true }
				network = &api.Network{
					ID: "net1",
					DriverState: &api.Driver{
						Name:    "overlay",
						Options: map[string]string{},
					},
					Spec: api.NetworkSpec{
						DriverConfig: &api.Driver{
							Name:    "overlay",
							Options: map[string]string{},
						},
						IPAM: &api.IPAMOptions{
							Driver: &api.Driver{
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
				}
				endpoint = &api.Endpoint{
					Spec:       &api.EndpointSpec{},
					VirtualIPs: []*api.Endpoint_VirtualIP{},
				}
				attachment = &api.NetworkAttachment{}
				netCopy = network.Copy()
				endCopy = endpoint.Copy()
				attachCopy = attachment.Copy()
				err = a.Restore([]*api.Network{netCopy}, []*api.Endpoint{endCopy}, []*api.NetworkAttachment{attachCopy})
			})
			It("should succeed", func() {
				Expect(err).ToNot(HaveOccurred())
			})
			It("should not not get an IPAM driver", func() {
				Expect(wasCalled).To(BeFalse())
			})
			It("should not modify the objects", func() {
				Expect(netCopy).To(Equal(network))
				Expect(endCopy).To(Equal(endpoint))
				Expect(attachCopy).To(Equal(attachment))
			})
		})
		Context("when a specified ipam driver is invalid", func() {
			var (
				err error
			)
			BeforeEach(func() {
				network := &api.Network{
					ID: "net1",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "doesnotexist",
						},
						Configs: []*api.IPAMConfig{
							{
								Family: api.IPAMConfig_IPV4,
							},
						},
					},
					Spec: api.NetworkSpec{
						IPAM: &api.IPAMOptions{
							Driver: &api.Driver{
								Name: "doesnotexist",
							},
						},
					},
				}

				err = a.Restore([]*api.Network{network}, nil, nil)
			})
			It("should return ErrInvalidIPAM", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(WithTransform(IsErrInvalidIPAM, BeTrue()))
				Expect(err.Error()).To(Equal("ipam driver doesnotexist for network net1 is not valid"))
			})
		})
		Context("when the IPAM fails to return a default address space", func() {
			// this case is unlikely but we test it in the interest of
			// completeness. i can basically only see it happening if a remote
			// ipam driver failed
			var (
				err error
			)
			BeforeEach(func() {
				reg.ipams["ipam"] = ipamAndCaps{
					ipam: &mockIpam{
						getDefaultAddressSpacesFunc: func() (string, string, error) {
							return "", "", fmt.Errorf("failed")
						},
					},
				}
				network := &api.Network{
					ID: "net2",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "ipam",
						},
						Configs: []*api.IPAMConfig{
							{
								Family: api.IPAMConfig_IPV4,
							},
						},
					},
					Spec: api.NetworkSpec{
						IPAM: &api.IPAMOptions{
							Driver: &api.Driver{
								Name: "ipam",
							},
						},
					},
				}
				err = a.Restore([]*api.Network{network}, nil, nil)
			})
			It("should return ErrBustedIPAM", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(WithTransform(IsErrBustedIPAM, BeTrue()))
				Expect(err.Error()).To(Equal("ipam error from driver ipam on network net2: failed"))
			})
		})
		Context("when objects are fully allocated", func() {
			var (
				err  error
				ipam *addressRestorerMockIpam
			)
			BeforeEach(func() {
				ipam = &addressRestorerMockIpam{
					&mockIpam{
						getDefaultAddressSpacesFunc: func() (string, string, error) {
							// we can return empty strings because we don't
							// actually in the code look at, care about, or use
							// the return value,
							return "", "", nil
						},
					},
					[]string{},
					[]struct{ subnet, iprange string }{},
				}
				reg.ipams["ipam"] = ipamAndCaps{
					ipam: ipam,
				}
				network1 := &api.Network{
					ID: "testID1",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "ipam",
						},
						Configs: []*api.IPAMConfig{
							{
								Subnet:  "192.168.1.0/24",
								Gateway: "192.168.1.1",
							},
						},
					},
					Spec: api.NetworkSpec{
						Annotations: api.Annotations{
							Name: "test1",
						},
						DriverConfig: &api.Driver{},
						IPAM: &api.IPAMOptions{
							Driver: &api.Driver{},
							Configs: []*api.IPAMConfig{
								{
									Subnet:  "192.168.1.0/24",
									Gateway: "192.168.1.1",
								},
							},
						},
					},
				}
				network2 := &api.Network{
					ID: "testID2",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "ipam",
						},
						Configs: []*api.IPAMConfig{
							{
								Subnet:  "192.168.2.0/24",
								Gateway: "192.168.2.1",
							},
						},
					},
					Spec: api.NetworkSpec{
						Annotations: api.Annotations{
							Name: "test2",
						},
						DriverConfig: &api.Driver{},
						IPAM: &api.IPAMOptions{
							Driver: &api.Driver{},
							Configs: []*api.IPAMConfig{
								{
									Subnet:  "192.168.2.0/24",
									Gateway: "192.168.2.1",
								},
							},
						},
					},
				}
				endpoint1 := &api.Endpoint{
					VirtualIPs: []*api.Endpoint_VirtualIP{
						{
							NetworkID: "testID1",
							Addr:      "192.168.1.2",
						},
					},
				}
				endpoint2 := &api.Endpoint{
					VirtualIPs: []*api.Endpoint_VirtualIP{
						{
							NetworkID: "testID1",
							Addr:      "192.168.1.3",
						},
						{
							NetworkID: "testID2",
							Addr:      "192.168.2.2",
						},
					},
				}
				attachment1 := &api.NetworkAttachment{
					Network:   network1,
					Addresses: []string{"192.168.1.4"},
				}
				attachment2 := &api.NetworkAttachment{
					Network:   network1,
					Addresses: []string{"192.168.1.5"},
				}
				attachment3 := &api.NetworkAttachment{
					Network:   network2,
					Addresses: []string{"192.168.2.3"},
				}
				err = a.Restore(
					[]*api.Network{network1, network2},
					[]*api.Endpoint{endpoint1, endpoint2},
					[]*api.NetworkAttachment{attachment1, attachment2, attachment3},
				)
			})
			It("should succeed", func() {
				Expect(err).ToNot(HaveOccurred())
			})
			It("should have requested 2 pools", func() {
				Expect(ipam.pools).To(HaveLen(2))
				Expect(ipam.pools[0].subnet).To(Equal("192.168.1.0/24"))
				Expect(ipam.pools[0].iprange).To(Equal(""))
				Expect(ipam.pools[1].subnet).To(Equal("192.168.2.0/24"))
				Expect(ipam.pools[1].iprange).To(Equal(""))
			})
			It("should have requested all of the IP address", func() {
				Expect(ipam.addresses).To(HaveLen(8))
				Expect(ipam.addresses).To(ConsistOf(
					"192.168.1.1gateway",
					"192.168.2.1gateway",
					"192.168.1.2",
					"192.168.1.3",
					"192.168.1.4",
					"192.168.1.5",
					"192.168.2.2",
					"192.168.2.3",
				))
			})
		})
	})
})
