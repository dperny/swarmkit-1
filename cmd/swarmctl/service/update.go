package service

import (
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"reflect"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/cmd/swarmctl/common"
	"github.com/docker/swarmkit/cmd/swarmctl/service/flagparser"
	"github.com/docker/swarmkit/specsignature"
	"github.com/spf13/cobra"
)

var (
	updateCmd = &cobra.Command{
		Use:   "update <service ID>",
		Short: "Update a service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("service ID missing")
			}

			if len(args) > 1 {
				return errors.New("update command takes exactly 1 argument")
			}

			c, err := common.Dial(cmd)
			if err != nil {
				return err
			}

			service, err := getService(common.Context(cmd), c, args[0])
			if err != nil {
				return err
			}

			spec := service.Spec.Copy()

			if err := flagparser.Merge(cmd, spec, c); err != nil {
				return err
			}

			if err := flagparser.ParseAddSecret(cmd, spec, "add-secret"); err != nil {
				return err
			}
			if err := flagparser.ParseRemoveSecret(cmd, spec, "rm-secret"); err != nil {
				return err
			}

			if err := flagparser.ParseAddConfig(cmd, spec, "add-config"); err != nil {
				return err
			}
			if err := flagparser.ParseRemoveConfig(cmd, spec, "rm-config"); err != nil {
				return err
			}

			if cmd.Flags().Changed("keyfile") {
				keyfile, err := cmd.Flags().GetString("keyfile")
				if err != nil {
					return err
				}
				key, err := common.GetSigningKey(keyfile)
				if err != nil {
					return err
				}
				specsignature.SignSpec(spec, key)
				clusters, err := c.ListClusters(common.Context(cmd), &api.ListClustersRequest{})
				vkey := clusters.Clusters[0].Spec.SpecSigningConfig.PublicKey
				fmt.Println("trying to verify with cluster's public key")
				vkeyrsa, err := x509.ParsePKIXPublicKey(vkey)
				if err != nil {
					return err
				}
				if err := specsignature.VerifySpec(spec, vkeyrsa.(*rsa.PublicKey)); err != nil {
					return fmt.Errorf("error verifying with cluster's key: %v", err)
				}
			}

			if reflect.DeepEqual(spec, &service.Spec) {
				return errors.New("no changes detected")
			}

			r, err := c.UpdateService(common.Context(cmd), &api.UpdateServiceRequest{
				ServiceID:      service.ID,
				ServiceVersion: &service.Meta.Version,
				Spec:           spec,
			})
			if err != nil {
				return err
			}
			fmt.Println(r.Service.ID)
			return nil
		},
	}
)

func init() {
	updateCmd.Flags().StringSlice("add-secret", nil, "add a new secret to the service")
	updateCmd.Flags().StringSlice("rm-secret", nil, "remove a secret from the service")
	updateCmd.Flags().StringSlice("add-config", nil, "add a new config to the service")
	updateCmd.Flags().StringSlice("rm-config", nil, "remove a config from the service")
	updateCmd.Flags().Bool("force", false, "force tasks to restart even if nothing has changed")
	flagparser.AddServiceFlags(updateCmd.Flags())
}
