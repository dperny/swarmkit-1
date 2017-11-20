package specsignature

import (
	"github.com/stretchr/testify/require"
	"testing"

	"crypto/rand"
	"crypto/rsa"

	"github.com/docker/swarmkit/api"
	"github.com/golang/protobuf/proto"
)

// keep a copy of the key to use in all tests because key generation is slow
var key *rsa.PrivateKey

func init() {
	// generate the rsa key
	var err error
	key, err = rsa.GenerateKey(rand.Reader, 512)
	if err != nil {
		panic(err)
	}
}

func TestSignServiceSpecValid(t *testing.T) {
	spec := &api.ServiceSpec{
		Annotations: api.Annotations{
			Name: "testname",
		},
		Task: api.TaskSpec{
			Runtime: &api.TaskSpec_Attachment{
				Attachment: &api.NetworkAttachmentSpec{
					ContainerID: "whatever",
				},
			},
		},
		Mode: &api.ServiceSpec_Global{
			Global: &api.GlobalService{},
		},
	}

	require.NoError(t, SignSpec(spec, key))
	require.NoError(t, VerifySpec(spec, &key.PublicKey))
}

func TestSignServiceSpecInvalid(t *testing.T) {
	spec := &api.ServiceSpec{
		Annotations: api.Annotations{
			Name: "testname",
		},
		Task: api.TaskSpec{
			Runtime: &api.TaskSpec_Attachment{
				Attachment: &api.NetworkAttachmentSpec{
					ContainerID: "whatever",
				},
			},
		},
		Mode: &api.ServiceSpec_Global{
			Global: &api.GlobalService{},
		},
	}

	require.NoError(t, SignSpec(spec, key))

	// Change the spec by altering the name.
	spec.Annotations.Name = "nesttame"
	// signature should not match
	require.Error(t, VerifySpec(spec, &key.PublicKey))
}

func TestSignServiceSpecSubspecsSigned(t *testing.T) {
	spec := &api.ServiceSpec{
		Annotations: api.Annotations{
			Name: "testname",
		},
		Task: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "nginx",
				},
			},
		},
		Mode: &api.ServiceSpec_Global{
			Global: &api.GlobalService{},
		},
		Endpoint: &api.EndpointSpec{
			Mode: api.ResolutionModeVirtualIP,
			Ports: []*api.PortConfig{
				{
					TargetPort:    80,
					PublishedPort: 8080,
				},
			},
		},
	}

	require.NoError(t, SignSpec(spec, key))
	// verify the subspecs
	for _, s := range []api.SignableSpec{
		// go inside to out
		spec.Task.GetContainer(),
		&spec.Task,
		spec.Endpoint,
		spec,
	} {
		t.Logf("Verifying %T", s)
		require.NoError(t, VerifySpec(s, &key.PublicKey))
	}
}

func TestSignServiceSpecSubspecsChangedInvalid(t *testing.T) {
	spec := &api.ServiceSpec{
		Annotations: api.Annotations{
			Name: "testname",
		},
		Task: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "nginx",
				},
			},
		},
		Mode: &api.ServiceSpec_Global{
			Global: &api.GlobalService{},
		},
		Endpoint: &api.EndpointSpec{
			Mode: api.ResolutionModeVirtualIP,
			Ports: []*api.PortConfig{
				{
					TargetPort:    80,
					PublishedPort: 8080,
				},
			},
		},
	}

	require.NoError(t, SignSpec(spec, key))
	// change the containerspec by adding somethign
	spec.Task.GetContainer().Env = []string{"SOMETHING=false"}
	require.Error(t, VerifySpec(spec, &key.PublicKey))
}

func TestSignServiceSpecMarshalUnmarshall(t *testing.T) {
	spec := &api.ServiceSpec{
		Annotations: api.Annotations{
			Name: "testname",
		},
		Task: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "nginx",
				},
			},
		},
		Mode: &api.ServiceSpec_Global{
			Global: &api.GlobalService{},
		},
		Endpoint: &api.EndpointSpec{
			Mode: api.ResolutionModeVirtualIP,
			Ports: []*api.PortConfig{
				{
					TargetPort:    80,
					PublishedPort: 8080,
				},
			},
		},
	}

	require.NoError(t, SignSpec(spec, key))
	// marshal the signed spec
	protobytes, err := proto.Marshal(spec)
	require.NoError(t, err)

	// create a new spec object and unmarshal into it
	newspec := &api.ServiceSpec{}
	require.NoError(t, proto.Unmarshal(protobytes, newspec))
	// verify that the signature still checks out
	require.NoError(t, VerifySpec(newspec, &key.PublicKey))
}
