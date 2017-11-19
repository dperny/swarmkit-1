package specsignature

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"

	"github.com/docker/swarmkit/api"
)

// SignSpec signs the spec with the provided key and adds it to the message
func SignSpec(spec api.SignableSpec, key *rsa.PrivateKey) error {
	if spec == nil {
		return errors.New("nil spec")
	}
	if spec.GetSignature() != nil {
		return errors.New("already signed")
	}
	// sign the subspecs
	for _, subspec := range spec.GetSubspecs() {
		// don't worry about the error here. The subcomponents might be nil or
		// already signed, and we don't care
		SignSpec(subspec, key)
	}

	// marshall the spec to bytes
	bytes, err := proto.Marshal(spec)
	if err != nil {
		return errors.Wrap(err, "error marshalling proto")
	}
	// hash the proto bytes
	digest := sha256.Sum256(bytes)
	// sign the hash
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return err
	}
	sigproto := &api.SpecSignature{Signature: sig}
	spec.SetSignature(sigproto)
	return nil
}

// VerifySpec verifies that the signature on the spec is valid. If the
// signature is invalid, an error will be returned. Otherwise, nil will be
// returned.
func VerifySpec(spec api.SignableSpec, key *rsa.PublicKey) error {
	if spec == nil {
		return errors.New("spec is nil")
	}
	if spec.GetSignature() == nil {
		return errors.New("spec has no signature")
	}

	// TODO(dperny) this signature swap thing is really bad, don't do it
	// basically, we need to compare the proto WITHOUT the signature, but we
	// want to put it back after
	sig := spec.GetSignature()
	spec.SetSignature(nil)
	defer func(sig *api.SpecSignature) { spec.SetSignature(sig) }(sig)

	// get the raw proto bytes
	bytes, err := proto.Marshal(spec)
	if err != nil {
		return errors.Wrap(err, "error marshalling proto")
	}
	digest := sha256.Sum256(bytes)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig.Signature)
}
