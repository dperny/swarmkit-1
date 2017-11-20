package store

import (
	"crypto/rsa"
	"crypto/x509"

	"github.com/pkg/errors"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/specsignature"
)

// VerifySpecInTx checks the cluster config to see if verification is enabled
// and then verifies the key is present. Returns an error if the spec cannot be
// verified
func VerifySpecInTx(tx ReadTx, spec api.SignableSpec) error {
	clusters, err := FindClusters(tx, ByName(DefaultClusterName))
	if err != nil {
		return err
	}
	cs := clusters[0].Spec
	if cs.SpecSigningConfig.Enable {
		// TODO(dperny): we don't want to unmarshal the key every time
		k, err := x509.ParsePKIXPublicKey(cs.SpecSigningConfig.PublicKey)
		if err != nil {
			return errors.Wrap(err, "error parsing SpecSigningConfig.PublicKey")
		}
		switch key := k.(type) {
		case *rsa.PublicKey:
			return specsignature.VerifySpec(spec, key)
		default:
			return errors.New("invalid public key type")
		}
	}
	return nil
}
