package network_test

import (
	. "github.com/docker/swarmkit/manager/allocator/network"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/manager/allocator/ipam"
)

type mockIpamAllocator struct{}

func (m *mockIpamAllocator) Restore(_ []*api.Network, _ []*api.Endpoint, _ []*api.NetworkAttachmentSpec) error {

}

var _ = Describe("network.Allocator", func() {
})
