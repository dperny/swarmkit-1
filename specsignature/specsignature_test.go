package specsignature

import (
	"github.com/stretchr/testify/require"
	"testing"

	"crypto/rand"
	"crypto/rsa"

	"github.com/docker/swarmkit/api"
)

func TestSignServiceSpec(t *testing.T) {
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

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	err = SignSpec(spec, key)
	require.NoError(t, err)

	err = VerifySpec(spec, &key.PublicKey)
	require.NoError(t, err)
}
