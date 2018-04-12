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
	if m.getDefaultAddressSpacesFunc == nil {
		// we can return empty strings because we don't actually in the code
		// look at, care about, or use the return value,
		return "", "", nil
	}
	return m.getDefaultAddressSpacesFunc()
}

func (m *mockIpam) RequestPool(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
	return m.requestPoolFunc(addressSpace, pool, subpool, options, v6)
}

func (m *mockIpam) ReleasePool(poolID string) error {
	if m.releasePoolFunc == nil {
		return nil
	}
	return m.releasePoolFunc(poolID)
}

func (m *mockIpam) RequestAddress(poolID string, ip net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	return m.requestAddressFunc(poolID, ip, opts)
}

func (m *mockIpam) ReleaseAddress(poolID string, ip net.IP) error {
	if m.releaseAddressFunc == nil {
		return nil
	}
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
		reg      *mockDrvRegistry
		a        Allocator
		restorer *addressRestorerMockIpam
	)
	BeforeEach(func() {
		reg = &mockDrvRegistry{
			ipams: make(map[string]ipamAndCaps),
			// add a noop before function so we don't try calling a nil
			beforeIpam: func() {},
		}
		// this is the default ipam we'll use most places, which successfully
		// "restores" addresses already requested
		restorer = &addressRestorerMockIpam{
			&mockIpam{},
			[]string{},
			[]struct{ subnet, iprange string }{},
		}
		reg.ipams["restore"] = ipamAndCaps{
			ipam: restorer,
		}
		reg.ipams["addressSpaceFails"] = ipamAndCaps{
			ipam: &mockIpam{
				getDefaultAddressSpacesFunc: func() (string, string, error) {
					return "", "", fmt.Errorf("failed")
				},
			},
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
				wasCalled = false
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
				network := &api.Network{
					ID: "net2",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "addressSpaceFails",
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
								Name: "addressSpaceFails",
							},
						},
					},
				}
				err = a.Restore([]*api.Network{network}, nil, nil)
			})
			It("should return ErrBustedIPAM", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(WithTransform(IsErrBustedIPAM, BeTrue()))
				Expect(err.Error()).To(Equal("ipam error from driver addressSpaceFails on network net2: failed"))
			})
		})
		Context("when objects are fully allocated", func() {
			var (
				err error
			)
			BeforeEach(func() {
				network1 := &api.Network{
					ID: "testID1",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "restore",
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
							Name: "restore",
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
				Expect(restorer.pools).To(HaveLen(2))
				Expect(restorer.pools[0].subnet).To(Equal("192.168.1.0/24"))
				Expect(restorer.pools[0].iprange).To(Equal(""))
				Expect(restorer.pools[1].subnet).To(Equal("192.168.2.0/24"))
				Expect(restorer.pools[1].iprange).To(Equal(""))
			})
			It("should have requested all of the IP address", func() {
				Expect(restorer.addresses).To(HaveLen(8))
				Expect(restorer.addresses).To(ConsistOf(
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
	Describe("allocating new networks", func() {
		Context("when the network is already allocated", func() {
			var (
				network *api.Network
				err     error
			)
			BeforeEach(func() {
				network = &api.Network{
					ID: "testID1",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: "restore",
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
							Driver: &api.Driver{
								Name: "restore",
							},
							Configs: []*api.IPAMConfig{
								{
									Subnet:  "192.168.1.0/24",
									Gateway: "192.168.1.1",
								},
							},
						},
					},
				}
				a.Restore([]*api.Network{network}, nil, nil)
				err = a.AllocateNetwork(network)
			})
			It("should return ErrNetworkAllocated", func() {
				Expect(err).To(HaveOccurred())
				Expect(err).To(WithTransform(IsErrNetworkAllocated, BeTrue()))
				Expect(err.Error()).To(Equal("network testID1 is already allocated and network updates are not supported"))
			})
		})
		Describe("a valid, correct allocation", func() {
			var (
				poolsRequested     int
				addressesRequested int
				mock               *mockIpam
			)
			BeforeEach(func() {
				poolsRequested = 0
				addressesRequested = 0
				// provide a mock ipam driver for the network
				mock = &mockIpam{
					requestPoolFunc: func(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
						poolsRequested = poolsRequested + 1
						return "pool1",
							&net.IPNet{net.IPv4(192, 168, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
							map[string]string{
								netlabel.Gateway: "192.168.2.1/24",
							},
							nil
					},
					requestAddressFunc: func(_ string, _ net.IP, _ map[string]string) (*net.IPNet, map[string]string, error) {
						addressesRequested = addressesRequested + 1
						return &net.IPNet{net.IPv4(192, 168, 2, 2), net.IPv4Mask(255, 255, 255, 128)}, nil, nil
					},
				}
				reg.ipams["default"] = ipamAndCaps{mock, nil}
			})
			AfterEach(func() {
				// we need to remove the mock IPAM driver we just created
				delete(reg.ipams, "default")
			})
			Context("when the user has specified no settings", func() {
				var (
					network *api.Network
					err     error
				)
				BeforeEach(func() {
					network = &api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							Annotations: api.Annotations{
								Name: "net1",
							},
						},
					}
					err = a.AllocateNetwork(network)
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should fill in the default driver settings", func() {
					Expect(network.IPAM).ToNot(BeNil())
					Expect(network.IPAM.Driver).ToNot(BeNil())
					Expect(network.IPAM.Driver.Name).To(Equal(ipamapi.DefaultIPAM))
					Expect(network.IPAM.Configs).To(HaveLen(1))
					Expect(network.IPAM.Configs[0]).ToNot(BeNil())
					Expect(network.IPAM.Configs[0].Family).To(Equal(api.IPAMConfig_IPV4))
					Expect(network.IPAM.Configs[0].Subnet).To(Equal("192.168.2.0/24"))
					Expect(network.IPAM.Configs[0].Gateway).To(Equal("192.168.2.1"))
				})
				It("should not alter the spec", func() {
					Expect(network.Spec).To(Equal(api.NetworkSpec{
						Annotations: api.Annotations{
							Name: "net1",
						},
					}))
				})
				It("should request 1 pool and no addresses", func() {
					Expect(poolsRequested).To(Equal(1))
					Expect(addressesRequested).To(Equal(0))
				})
			})
			Context("when the IPAM driver returns no gateway address", func() {
				var (
					network *api.Network
					err     error
				)
				BeforeEach(func() {
					mock.requestPoolFunc = func(_, _, _ string, _ map[string]string, _ bool) (string, *net.IPNet, map[string]string, error) {
						return "pool2",
							&net.IPNet{net.IPv4(192, 168, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
							map[string]string{},
							nil
					}
					network = &api.Network{
						ID: "net1",
					}
					err = a.AllocateNetwork(network)
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should request a gateway address for the network", func() {
					Expect(addressesRequested).To(Equal(1))
					Expect(network.IPAM).ToNot(BeNil())
					Expect(network.IPAM.Configs).ToNot(BeEmpty())
					Expect(network.IPAM.Configs[0]).ToNot(BeNil())
					Expect(network.IPAM.Configs[0].Subnet).To(Equal("192.168.2.0/24"))
					Expect(network.IPAM.Configs[0].Gateway).To(Equal("192.168.2.2"))
				})
			})
			Context("when a gateway address is specified by the user", func() {
				var (
					addressRequested string
					network          *api.Network
					err              error
				)
				BeforeEach(func() {
					addressRequested = ""
					mock.requestAddressFunc = func(_ string, address net.IP, _ map[string]string) (*net.IPNet, map[string]string, error) {
						addressesRequested = addressesRequested + 1
						addressRequested = address.String()
						return &net.IPNet{address, address.DefaultMask()}, nil, nil
					}
					network = &api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Configs: []*api.IPAMConfig{
									{
										Gateway: "192.168.2.99",
									},
								},
							},
						},
					}
					err = a.AllocateNetwork(network)
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should use the spec's gateway address and not the IPAM driver's", func() {
					Expect(addressRequested).To(Equal("192.168.2.99"))
					Expect(network.IPAM).ToNot(BeNil())
					Expect(network.IPAM.Configs).ToNot(BeEmpty())
					Expect(network.IPAM.Configs[0]).ToNot(BeNil())
					Expect(network.IPAM.Configs[0].Subnet).To(Equal("192.168.2.0/24"))
					Expect(network.IPAM.Configs[0].Gateway).To(Equal("192.168.2.99"))
				})
			})
			Context("when specifying an IPAM driver", func() {
				var (
					network, nwCopy *api.Network
					err             error
					wasCalled       bool
				)
				BeforeEach(func() {
					wasCalled = false
					mock := &mockIpam{
						requestPoolFunc: func(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
							wasCalled = true
							return "nondefaultpool",
								&net.IPNet{net.IPv4(192, 168, 10, 0), net.IPv4Mask(255, 255, 255, 0)},
								map[string]string{
									netlabel.Gateway: "192.168.10.1/24",
								},
								nil
						},
					}
					reg.ipams["nondefault"] = ipamAndCaps{mock, nil}
					network = &api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							Annotations: api.Annotations{
								Name: "net1",
							},
							IPAM: &api.IPAMOptions{
								Driver: &api.Driver{
									Name: "nondefault",
								},
							},
						},
					}
					nwCopy = network.Copy()
					err = a.AllocateNetwork(network)
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should use the requested ipam driver", func() {
					Expect(network.IPAM).ToNot(BeNil())
					Expect(network.IPAM.Driver).ToNot(BeNil())
					Expect(network.IPAM.Driver.Name).To(Equal("nondefault"))
					Expect(network.IPAM.Configs).ToNot(BeNil())
					Expect(network.IPAM.Configs).ToNot(BeEmpty())
					Expect(network.IPAM.Configs[0].Gateway).To(Equal("192.168.10.1"))
					Expect(wasCalled).To(BeTrue())
				})
			})
			Context("when specifying IPAM driver options", func() {
				var (
					network           *api.Network
					err               error
					options           map[string]string
					calledWithOptions map[string]string
				)
				BeforeEach(func() {
					calledWithOptions = nil
					options = map[string]string{"foo": "bar", "baz": "bat"}
					network = &api.Network{
						ID: "id1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Driver: &api.Driver{
									Options: options,
								},
							},
						},
					}
					mock.requestPoolFunc = func(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
						calledWithOptions = options
						poolsRequested = poolsRequested + 1
						return "pool1",
							&net.IPNet{net.IPv4(192, 168, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
							map[string]string{
								netlabel.Gateway: "192.168.2.1/24",
							},
							nil
					}
					err = a.AllocateNetwork(network)
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should use the provided options when allocating", func() {
					Expect(calledWithOptions).To(Equal(options))
				})
				It("should fill in the Options field on the IPAM.Driver with the provided options", func() {
					Expect(network.IPAM).ToNot(BeNil())
					Expect(network.IPAM.Driver).ToNot(BeNil())
					Expect(network.IPAM.Driver.Options).To(Equal(options))
				})
			})
		})
		Describe("a failed request", func() {
			Context("when passing an invalid ipam", func() {
				var (
					network, nwCopy *api.Network
					err             error
				)
				BeforeEach(func() {
					network = &api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Driver: &api.Driver{
									Name: "invalid",
								},
							},
						},
					}
					nwCopy = network.Copy()
					err = a.AllocateNetwork(network)
				})
				It("should fail with ErrInvalidIPAM", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrInvalidIPAM, BeTrue()))
					Expect(err.Error()).To(Equal("ipam driver invalid for network net1 is not valid"))
				})
				It("should not alter the network object", func() {
					Expect(network).To(Equal(nwCopy))
				})
			})
			Context("when the IPAM driver fails to return an address space", func() {
				// again, testing this is just... pretty dumb, i don't know why
				// this function is ALLOWED to fail...
				var (
					err error
				)
				BeforeEach(func() {
					net := &api.Network{
						ID: "net2",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Driver: &api.Driver{
									Name: "addressSpaceFails",
								},
							},
						},
					}
					err = a.AllocateNetwork(net)
				})
				It("should return ErrBustedIPAM", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrBustedIPAM, BeTrue()))
					Expect(err.Error()).To(Equal("ipam error from driver addressSpaceFails on network net2: failed"))
				})
			})
			Context("when the pool request fails", func() {
				var (
					wasCalled bool
					err       error
				)
				BeforeEach(func() {
					wasCalled = false
					mock := &mockIpam{
						requestPoolFunc: func(_, _, _ string, _ map[string]string, _ bool) (string, *net.IPNet, map[string]string, error) {
							wasCalled = true
							return "", nil, nil, fmt.Errorf("failed")
						},
					}
					reg.ipams["poolfails"] = ipamAndCaps{mock, nil}
					err = a.AllocateNetwork(&api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Driver: &api.Driver{
									Name: "poolfails",
								},
							},
						},
					})
				})
				AfterEach(func() {
					delete(reg.ipams, "poolfails")
				})
				It("should fail with ErrFailedPoolRequest", func() {
					Expect(wasCalled).To(BeTrue())
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrFailedPoolRequest, BeTrue()))
					Expect(err.Error()).To(Equal("requesting pool (subnet: \"\", range: \"\") returned error: failed"))
				})
			})
			Context("when the IPAM driver returns an invalid gateway address", func() {
				// why is this even possible tho i don't even understand why we
				// need to check this
				var (
					err            error
					poolsRequested int
				)
				BeforeEach(func() {
					poolsRequested = 0
					mock := &mockIpam{
						requestPoolFunc: func(addressSpace, pool, subpool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
							poolsRequested = poolsRequested + 1
							return "pool1",
								&net.IPNet{net.IPv4(192, 168, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
								map[string]string{
									netlabel.Gateway: "notvalid",
								},
								nil
						},
					}
					reg.ipams["default"] = ipamAndCaps{mock, nil}
					network := &api.Network{
						ID: "net1",
						Spec: api.NetworkSpec{
							Annotations: api.Annotations{
								Name: "net1",
							},
						},
					}
					err = a.AllocateNetwork(network)
				})
				AfterEach(func() {
					delete(reg.ipams, "default")
				})
				It("should return ErrBustedIPAM", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrBustedIPAM, BeTrue()))
					Expect(err.Error()).To(Equal("ipam error from driver default on network net1: can't parse gateway address (notvalid) returned by the ipam driver: invalid CIDR address: notvalid"))
				})
			})
			Context("when requesting a gateway address fails", func() {
				var (
					err error
				)
				BeforeEach(func() {
					mock := &mockIpam{
						requestPoolFunc: func(_, _, _ string, _ map[string]string, _ bool) (string, *net.IPNet, map[string]string, error) {
							return "pool1",
								&net.IPNet{net.IPv4(192, 168, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
								map[string]string{
									netlabel.Gateway: "192.168.2.1/24",
								},
								nil
						},
						requestAddressFunc: func(_ string, _ net.IP, _ map[string]string) (*net.IPNet, map[string]string, error) {
							return nil, nil, fmt.Errorf("failed")
						},
					}
					reg.ipams["default"] = ipamAndCaps{mock, nil}
					err = a.AllocateNetwork(&api.Network{
						ID: "id1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Configs: []*api.IPAMConfig{
									{
										Gateway: "192.168.2.11",
									},
								},
							},
						},
					})
				})
				AfterEach(func() {
					delete(reg.ipams, "default")
				})
				It("should return ErrFailedAddressRequest", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrFailedAddressRequest, BeTrue()))
					Expect(err.Error()).To(Equal("requesting address 192.168.2.11 failed: failed"))
				})
			})
			Context("when multiple configs are specified, and a later one fails", func() {
				var (
					mock                             *mockIpam
					network, nwCopy                  *api.Network
					poolsReleased, addressesReleased int
					err                              error
				)
				BeforeEach(func() {
					poolsReleased = 0
					addressesReleased = 0
					var i byte = 0
					mock = &mockIpam{
						requestPoolFunc: func(_, _, _ string, _ map[string]string, _ bool) (string, *net.IPNet, map[string]string, error) {
							i = i + 1
							return fmt.Sprintf("pool%v", i),
								&net.IPNet{net.IPv4(192, 168, i, 0), net.IPv4Mask(255, 255, 255, 0)},
								map[string]string{
									netlabel.Gateway: fmt.Sprintf("192.168.%v.1/24", i),
								},
								nil
						},
						requestAddressFunc: func(_ string, _ net.IP, _ map[string]string) (*net.IPNet, map[string]string, error) {
							return nil, nil, fmt.Errorf("failed")
						},
						releasePoolFunc: func(_ string) error {
							poolsReleased = poolsReleased + 1
							return nil
						},
						releaseAddressFunc: func(_ string, _ net.IP) error {
							addressesReleased = addressesReleased + 1
							return nil
						},
					}
					reg.ipams["default"] = ipamAndCaps{mock, nil}
					// we're going to force this behavior by not allocating a
					// gateway for the first config, but allocating one for
					// the second config. that will cause ipam.RequestAddress
					// to run in the second loop iteration, which will return
					// an error
					network = &api.Network{
						ID: "id1",
						Spec: api.NetworkSpec{
							IPAM: &api.IPAMOptions{
								Configs: []*api.IPAMConfig{
									{
										Family: api.IPAMConfig_IPV4,
									},
									{
										Gateway: "192.168.2.108",
									},
								},
							},
						},
					}
					nwCopy = network.Copy()
				})
				// using JustBeforeEach here so we can check the sub-cases of
				// double faults
				JustBeforeEach(func() {
					err = a.AllocateNetwork(network)
				})
				It("should fail with ErrFailedAddressRequest", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrFailedAddressRequest, BeTrue()))
				})
				It("should not alter the network object", func() {
					Expect(network).To(Equal(nwCopy))
				})
				It("should the release 2 pools", func() {
					Expect(poolsReleased).To(Equal(2))
				})
				It("should release 1 address", func() {
					Expect(addressesReleased).To(Equal(1))
				})
				Context("when a double fault occurs releasing pools", func() {
					BeforeEach(func() {
						mock.releasePoolFunc = func(_ string) error {
							return fmt.Errorf("failed")
						}
					})
					It("should fail with ErrDoubleFault", func() {
						Expect(err).To(HaveOccurred())
						Expect(err).To(WithTransform(IsErrDoubleFault, BeTrue()))
						Expect(err.Error()).To(Equal(
							"double fault: an error occurred while handling error requesting address 192.168.2.108 failed: failed: failed"))
						e := err.(ErrDoubleFault)
						Expect(e.Original()).ToNot(BeNil())
						Expect(e.Original()).To(WithTransform(IsErrFailedAddressRequest, BeTrue()))
						Expect(e.New()).ToNot(BeNil())
						Expect(e.New()).To(Equal(fmt.Errorf("failed")))
					})
				})
				Context("when a double fault occurs releasing addresses", func() {
					BeforeEach(func() {
						mock.releaseAddressFunc = func(_ string, _ net.IP) error {
							return fmt.Errorf("failed")
						}
					})
					It("should fail with ErrDoubleFault", func() {
						Expect(err).To(HaveOccurred())
						Expect(err).To(WithTransform(IsErrDoubleFault, BeTrue()))
						e := err.(ErrDoubleFault)
						Expect(e.Original()).ToNot(BeNil())
						Expect(e.Original()).To(WithTransform(IsErrFailedAddressRequest, BeTrue()))
						Expect(e.New()).ToNot(BeNil())
						Expect(e.New()).To(Equal(fmt.Errorf("failed")))
					})
				})
			})
		})
	})

	Describe("allocating VIPs and Attachments", func() {
		var (
			addressesAllocated int
			// addressesReleased contains all of the addresses released and
			// their pool IDs
			addressesReleased map[string]net.IP
		)
		BeforeEach(func() {
			addressesAllocated = 0
			addressesReleased = map[string]net.IP{}

			// set up some networks and mocks. allocating VIPs and attachments
			// requires a lot more dependent state than allocating Networks
			// does, so we'll set it all up here. We can call Restore any
			// number of times, so it doesn't matter if a later test calls
			// Restore again
			networks := []*api.Network{
				{
					ID: "nw1",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name: ipamapi.DefaultIPAM,
						},
						Configs: []*api.IPAMConfig{
							{
								Subnet:  "192.168.0.1/24",
								Range:   "192.168.0.1/24",
								Gateway: "192.168.0.1",
							},
							{
								Subnet:  "192.168.1.1/24",
								Range:   "192.168.1.1/24",
								Gateway: "192.168.1.1",
							},
						},
					},
				},
				{
					ID: "nw2",
					IPAM: &api.IPAMOptions{
						Driver: &api.Driver{
							Name:    ipamapi.DefaultIPAM,
							Options: map[string]string{"foo": "bar"},
						},
						Configs: []*api.IPAMConfig{
							{
								Subnet:  "192.168.2.1/24",
								Range:   "192.168.2.1/24",
								Gateway: "192.168.2.1",
							},
						},
					},
				},
			}
			// make a mock IPAM driver that can allocate new addresses.
			mockDefaultIpam := &mockIpam{
				requestAddressFunc: func(_ string, addr net.IP, _ map[string]string) (*net.IPNet, map[string]string, error) {
					// it doesn't matter if the address is literally anywhere
					// near correct or real. we don't have to reproduce the
					// actual behavior of an IPAM driver. the only behavior
					// that actually matters is returning valid IP addresses
					// that are different from one another
					ip := net.IPv4(192, 168, 3, byte(addressesAllocated))
					addressesAllocated = addressesAllocated + 1
					return &net.IPNet{ip, ip.DefaultMask()}, nil, nil
				},
				releaseAddressFunc: func(pool string, addr net.IP) error {
					addressesReleased[pool] = addr
					return nil
				},
			}

			// swap out the default mock IPAM for the restorer mock IPAM for
			// just long enough to restore all of this
			reg.ipams[ipamapi.DefaultIPAM] = ipamAndCaps{restorer, nil}
			defer func() {
				reg.ipams[ipamapi.DefaultIPAM] = ipamAndCaps{mockDefaultIpam, nil}
			}()
			a.Restore(networks, nil, nil)
		})
		Describe("allocating vips", func() {
			Context("when a requested network is not yet allocated", func() {
				var err error
				BeforeEach(func() {
					e := &api.Endpoint{}
					err = a.AllocateVIPs(e, []string{"notreal"})
				})
				It("should fail with ErrNetworkNotAllocated", func() {
					Expect(err).To(HaveOccurred())
					Expect(err).To(WithTransform(IsErrNetworkNotAllocated, BeTrue()))
					Expect(err.Error()).To(Equal("network notreal is not allocated"))
				})
			})
			Context("to a new endpoint", func() {
				var (
					endpoint *api.Endpoint
					err      error
				)
				BeforeEach(func() {
					endpoint = &api.Endpoint{}
					err = a.AllocateVIPs(endpoint, []string{"nw1", "nw2"})
				})
				It("should succeed", func() {
					Expect(err).ToNot(HaveOccurred())
				})
				It("should add VIPs to the endpoint", func() {
					Expect(endpoint.VirtualIPs).ToNot(BeNil())
					Expect(endpoint.VirtualIPs).To(HaveLen(2))
					Expect(endpoint.VirtualIPs).To(ConsistOf(
						&api.Endpoint_VirtualIP{"nw1", "192.168.3.0/24"},
						&api.Endpoint_VirtualIP{"nw2", "192.168.3.1/24"},
					))
				})
				It("should have allocated 2 addresses", func() {
					Expect(addressesAllocated).To(Equal(2))
				})
			})
			Context("when updating the attached networks", func() {
				var (
					endpoint *api.Endpoint
				)
				BeforeEach(func() {
					endpoint = &api.Endpoint{
						VirtualIPs: []*api.Endpoint_VirtualIP{
							{
								NetworkID: "nw1",
								Addr:      "192.168.0.2/24",
							},
						},
					}
					// swap out the restorer ipam for a second
					def := reg.ipams[ipamapi.DefaultIPAM]
					reg.ipams[ipamapi.DefaultIPAM] = ipamAndCaps{restorer, nil}
					defer func() {
						reg.ipams[ipamapi.DefaultIPAM] = def
					}()
					a.Restore(nil, []*api.Endpoint{endpoint}, nil)
				})
				Context("to add more networks", func() {
					var (
						err error
					)
					BeforeEach(func() {
						err = a.AllocateVIPs(endpoint, []string{"nw1", "nw2"})
					})
					It("should succeed", func() {
						Expect(err).ToNot(HaveOccurred())
					})
					It("should add one vip to the endpoint", func() {
						Expect(addressesAllocated).To(Equal(1))
						Expect(endpoint.VirtualIPs).ToNot(BeNil())
						Expect(endpoint.VirtualIPs).To(HaveLen(2))
						Expect(endpoint.VirtualIPs).To(ConsistOf(
							&api.Endpoint_VirtualIP{"nw1", "192.168.0.2/24"},
							&api.Endpoint_VirtualIP{"nw2", "192.168.3.0/24"},
						))
					})
				})
				Context("to remove a network", func() {
					var (
						err error
					)
					BeforeEach(func() {
						err = a.AllocateVIPs(endpoint, []string{})
					})
					It("should succeed", func() {
						Expect(err).ToNot(HaveOccurred())
					})
					It("should deallocate 1 address", func() {
						Expect(endpoint.VirtualIPs).To(HaveLen(0))
						Expect(addressesReleased).To(HaveLen(1))
						// NOTE(dperny): this is pretty sloppy because i'm
						// relying on the fact that I know this IP address
						// belongs to pool 1 because i know how the
						// addressRestorerMockIpam works
						Expect(addressesReleased).To(HaveKey("2"))
						addr, _, _ := net.ParseCIDR("192.168.0.2/24")
						Expect(addressesReleased["2"]).To(Equal(addr))
					})
				})
				Context("to both add and remove a network", func() {
					var (
						err error
					)
					BeforeEach(func() {
						err = a.AllocateVIPs(endpoint, []string{"nw2"})
					})
					It("should succeed", func() {
						Expect(err).ToNot(HaveOccurred())
					})
					It("should add one vip to the endpoint", func() {
						Expect(addressesAllocated).To(Equal(1))
						Expect(endpoint.VirtualIPs).To(HaveLen(1))
						Expect(endpoint.VirtualIPs).To(ConsistOf(
							&api.Endpoint_VirtualIP{"nw2", "192.168.3.0/24"},
						))
					})
					It("should deallocate one vip", func() {
						Expect(addressesReleased).To(HaveLen(1))
						Expect(addressesReleased).To(HaveKey("2"))
						addr, _, _ := net.ParseCIDR("192.168.0.2/24")
						Expect(addressesReleased["2"]).To(Equal(addr))
					})
				})
			})
		})
	})
})
