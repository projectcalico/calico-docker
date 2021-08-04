// Copyright (c) 2016 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"context"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	"k8s.io/apimachinery/pkg/util/json"

	docopt "github.com/docopt/docopt-go"

	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/argutils"
	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/clientmgr"
	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/common"
	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/constants"
	"github.com/projectcalico/calicoctl/v3/calicoctl/util"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
)

// IPAM takes keyword with an IP address then calls the subcommands.
func Release(args []string, version string) error {
	doc := constants.DatastoreIntro + `Usage:
  <BINARY_NAME> ipam release [--ip=<IP>] [--from-report=<REPORT>] [--config=<CONFIG>] [--force] [--allow-version-mismatch]

Options:
  -h --help                   Show this screen.
     --ip=<IP>                IP address to release.
     --from-report=<REPORT>   Release all leaked addresses from the report.
     --force                  Force release of leaked addresses.
  -c --config=<CONFIG>        Path to the file containing connection configuration in
                              YAML or JSON format.
                              [default: ` + constants.DefaultConfigPath + `]
  --allow-version-mismatch    Allow client and cluster versions mismatch.

Description:
  The ipam release command releases an IP address from the Calico IP Address
  Manager that was been previously assigned to an endpoint.  When an IP address
  is released, it becomes available for assignment to any endpoint.

  Note that this does not remove the IP from any existing endpoints that may be
  using it, so only use this command to clean up addresses from endpoints that
  were not cleanly removed from Calico.
`
	// Replace all instances of BINARY_NAME with the name of the binary.
	name, _ := util.NameAndDescription()
	doc = strings.ReplaceAll(doc, "<BINARY_NAME>", name)

	parsedArgs, err := docopt.ParseArgs(doc, args, "")
	if err != nil {
		return fmt.Errorf("Invalid option: 'calicoctl %s'. Use flag '--help' to read about a specific subcommand.", strings.Join(args, " "))
	}
	if len(parsedArgs) == 0 {
		return nil
	}

	common.CheckVersionMismatch(parsedArgs["--config"], parsedArgs["--allow-version-mismatch"])

	ctx := context.Background()

	// Load config.
	cf := parsedArgs["--config"].(string)
	cfg, err := clientmgr.LoadClientConfig(cf)
	if err != nil {
		return err
	}

	// Set QPS - we want to increase this because we may need to send many IPAM requests
	// in a short period of time in order to release a large number of addresses.
	cfg.Spec.K8sClientQPS = float32(100)

	// Create a new backend client.
	client, err := clientmgr.NewClientFromConfig(cfg)
	if err != nil {
		return err
	}

	ipamClient := client.IPAM()

	if report := parsedArgs["--from-report"]; report != nil {
		reportFile := parsedArgs["--from-report"].(string)
		force := false
		if parsedArgs["--force"] != nil {
			force = parsedArgs["--force"].(bool)
		}
		err = releaseFromReport(ctx, client, force, reportFile, version)
		if err != nil {
			return err
		}
		fmt.Println("You may now unlock the data store.")
		return nil
	}

	if ip := parsedArgs["--ip"]; ip != nil {
		passedIP := parsedArgs["--ip"].(string)
		ip := argutils.ValidateIP(passedIP)
		ips := []net.IP{ip}

		// Call ReleaseIPs releases the IP and returns an empty slice as unallocatedIPs if
		// release was successful else it returns back the slice with the IP passed in.
		unallocatedIPs, err := ipamClient.ReleaseIPs(ctx, ips)
		if err != nil {
			return fmt.Errorf("Error: %v", err)
		}

		// Couldn't release the IP if the slice is not empty or IP might already be released/unassigned.
		// This is not exactly an error, so not returning it to the caller.
		if len(unallocatedIPs) != 0 {
			return fmt.Errorf("IP address %s is not assigned", ip)
		}

		// If unallocatedIPs slice is empty then IP was released Successfully.
		fmt.Printf("Successfully released IP address %s\n", ip)
	}

	return nil
}

func releaseFromReport(ctx context.Context, c client.Interface, force bool, reportFile string, version string) error {
	// Load the report into memory.
	r := Report{}
	bytes, err := ioutil.ReadFile(reportFile)
	if err != nil {
		return err
	}
	err = json.Unmarshal(bytes, &r)
	if err != nil {
		return err
	}

	// Make sure the metadata from the report matches the cluster.
	clusterInfo, err := c.ClusterInformation().Get(ctx, "default", options.GetOptions{})
	if err != nil {
		return err
	}
	if clusterInfo.Spec.ClusterGUID != r.ClusterGUID {
		// This check cannot be overridden using the --force option, because it is critical.
		return fmt.Errorf("Cluster does not match the provided report: mismatched cluster GUID. Refusing to release.")
	}
	if clusterInfo.ResourceVersion != r.ClusterInfoRevision {
		return fmt.Errorf("The provided report is stale, please generate a new report while the data store is locked and try again.")
	}
	if clusterInfo.Spec.DatastoreReady == nil || *clusterInfo.Spec.DatastoreReady {
		if !force {
			return fmt.Errorf("Data store is not locked. Either lock the data store, or re-run with --force.")
		} else {
			fmt.Println("WARNING: Data store is not locked. Ignoring due to --force option")
		}
	}
	if version != r.Version {
		if !force {
			return fmt.Errorf("The provided report was produced using a different version (%s) of calicoctl. Refusing to release.", r.Version)
		} else {
			fmt.Println("WARNING: Report was produced using a different version of calicoctl. Ignoring due to --force option")
		}
	}

	// For each address that needs to be released, do so.
	ipsToRelease := []net.IP{}
	for _, allocations := range r.Allocations {
		for _, a := range allocations {
			if !a.InUse {
				ipsToRelease = append(ipsToRelease, argutils.ValidateIP(a.IP))
			}
		}
	}

	if len(ipsToRelease) == 0 {
		fmt.Println("No addresses need to be released.")
		return nil
	}
	fmt.Printf("Releasing %d old IPs\n", len(ipsToRelease))

	unallocated, err := c.IPAM().ReleaseIPs(ctx, ipsToRelease)
	if err != nil {
		return err
	}
	if len(unallocated) != 0 {
		fmt.Println("Warning: report contained addresses which are no longer allocated")
	} else {
		fmt.Printf("Released %d IPs successfully\n", len(ipsToRelease))
	}

	return nil
}
