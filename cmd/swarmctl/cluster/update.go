package cluster

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/cli"
	"github.com/docker/swarmkit/cmd/swarmctl/common"
	"github.com/docker/swarmkit/specsignature"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/spf13/cobra"
)

var (
	externalCAOpt cli.ExternalCAOpt

	updateCmd = &cobra.Command{
		Use:   "update <cluster name>",
		Short: "Update a cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("cluster name missing")
			}

			if len(args) > 1 {
				return errors.New("update command takes exactly 1 argument")
			}

			c, err := common.Dial(cmd)
			if err != nil {
				return err
			}

			cluster, err := getCluster(common.Context(cmd), c, args[0])
			if err != nil {
				return err
			}

			flags := cmd.Flags()
			spec := &cluster.Spec
			var rotation api.KeyRotation

			if flags.Changed("certexpiry") {
				cePeriod, err := flags.GetDuration("certexpiry")
				if err != nil {
					return err
				}
				ceProtoPeriod := gogotypes.DurationProto(cePeriod)
				spec.CAConfig.NodeCertExpiry = ceProtoPeriod
			}
			if flags.Changed("external-ca") {
				spec.CAConfig.ExternalCAs = externalCAOpt.Value()
			}
			if flags.Changed("taskhistory") {
				taskHistory, err := flags.GetInt64("taskhistory")
				if err != nil {
					return err
				}
				spec.Orchestration.TaskHistoryRetentionLimit = taskHistory
			}
			if flags.Changed("heartbeatperiod") {
				hbPeriod, err := flags.GetDuration("heartbeatperiod")
				if err != nil {
					return err
				}
				spec.Dispatcher.HeartbeatPeriod = gogotypes.DurationProto(hbPeriod)
			}
			if flags.Changed("rotate-join-token") {
				rotateJoinToken, err := flags.GetString("rotate-join-token")
				if err != nil {
					return err
				}
				rotateJoinToken = strings.ToLower(rotateJoinToken)

				switch rotateJoinToken {
				case "worker":
					rotation.WorkerJoinToken = true
				case "manager":
					rotation.ManagerJoinToken = true
				default:
					return errors.New("--rotate-join-token flag must be followed by 'worker' or 'manager'")
				}
			}
			if flags.Changed("autolock") {
				spec.EncryptionConfig.AutoLockManagers, err = flags.GetBool("autolock")
				if err != nil {
					return err
				}
			}
			rotateUnlockKey, err := flags.GetBool("rotate-unlock-key")
			if err != nil {
				return err
			}
			rotation.ManagerUnlockKey = rotateUnlockKey

			driver, err := common.ParseLogDriverFlags(flags)
			if err != nil {
				return err
			}
			spec.TaskDefaults.LogDriver = driver

			if flags.Changed("enable-spec-signing") {
				fmt.Println("disabling spec signing")
				e, err := flags.GetBool("enable-spec-signing")
				if err != nil {
					return err
				}
				spec.SpecSigningConfig.Enable = e
			}
			if flags.Changed("spec-signing-keyfile") {
				f, err := flags.GetString("spec-signing-keyfile")
				if err != nil {
					return err
				}
				fh, err := os.Open(f)
				if err != nil {
					return err
				}
				pemfile, err := ioutil.ReadAll(fh)
				if err != nil {
					return err
				}
				k, _ := pem.Decode(pemfile)
				if k == nil {
					return errors.New("invalid pemfile")
				}

				spec.SpecSigningConfig.PublicKey = k.Bytes
			}

			if cluster.Spec.SpecSigningConfig.Enable || spec.SpecSigningConfig.Enable || true {
				fmt.Println("checking cluster verifies with own key...")
				cpk, err := x509.ParsePKIXPublicKey(cluster.Spec.SpecSigningConfig.PublicKey)
				if err := specsignature.VerifySpec(&cluster.Spec, cpk.(*rsa.PublicKey)); err != nil {
					return fmt.Errorf("Cluster does not verify with own key: %v", err)
				}
				fmt.Println("trying to verify with cluster's public key")
				file, err := flags.GetString("keyfile")
				if err != nil {
					return err
				}
				key, err := common.GetSigningKey(file)
				if err != nil {
					return err
				}
				specsignature.SignSpec(spec, key)
				vkey := cluster.Spec.SpecSigningConfig.PublicKey
				fmt.Println("trying to verify with cluster's public key")
				pem.Encode(os.Stdout, &pem.Block{Bytes: vkey})
				vkeyrsa, err := x509.ParsePKIXPublicKey(vkey)
				if err != nil {
					return err
				}
				if err := specsignature.VerifySpec(spec, vkeyrsa.(*rsa.PublicKey)); err != nil {
					return fmt.Errorf("error verifying with cluster's key: %v", err)
				}
				fmt.Println("verifies with cluster's public key...")
			}

			r, err := c.UpdateCluster(common.Context(cmd), &api.UpdateClusterRequest{
				ClusterID:      cluster.ID,
				ClusterVersion: &cluster.Meta.Version,
				Spec:           spec,
				Rotation:       rotation,
			})
			if err != nil {
				return err
			}
			fmt.Println(r.Cluster.ID)

			if rotation.ManagerUnlockKey {
				return displayUnlockKey(cmd)
			}
			return nil
		},
	}
)

func init() {
	updateCmd.Flags().Int64("taskhistory", 0, "Number of historic task entries to retain per slot or node")
	updateCmd.Flags().Duration("certexpiry", 24*30*3*time.Hour, "Duration node certificates will be valid for")
	updateCmd.Flags().Var(&externalCAOpt, "external-ca", "Specifications of one or more certificate signing endpoints")
	updateCmd.Flags().Duration("heartbeatperiod", 0, "Period when heartbeat is expected to receive from agent")

	updateCmd.Flags().String("log-driver", "", "Set default log driver for cluster")
	updateCmd.Flags().StringSlice("log-opt", nil, "Set options for default log driver")
	updateCmd.Flags().String("rotate-join-token", "", "Rotate join token for worker or manager")
	updateCmd.Flags().Bool("rotate-unlock-key", false, "Rotate manager unlock key")
	updateCmd.Flags().Bool("autolock", false, "Enable or disable manager autolocking (requiring an unlock key to start a stopped manager)")
	updateCmd.Flags().Bool("enable-spec-signing", false, "Enable spec signing")
	updateCmd.Flags().String("spec-signing-keyfile", "", "Spec signing public key file as a pem")
}
