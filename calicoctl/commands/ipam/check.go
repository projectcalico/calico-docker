// Copyright (c) 2016-2020 Tigera, Inc. All rights reserved.

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
	"net"
	"sort"
	"strings"

	docopt "github.com/docopt/docopt-go"
	"k8s.io/client-go/kubernetes"

	"github.com/projectcalico/libcalico-go/lib/ipam"

	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/clientv3"
	cnet "github.com/projectcalico/libcalico-go/lib/net"

	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/options"

	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/constants"
	"github.com/projectcalico/calicoctl/v3/calicoctl/util"

	"github.com/projectcalico/calicoctl/v3/calicoctl/commands/clientmgr"
)

// IPAM takes keyword with an IP address then calls the subcommands.
func Check(args []string) error {
	doc := constants.DatastoreIntro + `Usage:
  <BINARY_NAME> ipam check [--config=<CONFIG>] [--show-all-ips] [--show-problem-ips]

Options:
  -h --help                Show this screen.
  -c --config=<CONFIG>     Path to the file containing connection configuration in
                           YAML or JSON format.
                           [default: ` + constants.DefaultConfigPath + `]

Description:
  The ipam check command checks the integrity of the IPAM datastructures against Kubernetes.
`
	// Replace all instances of BINARY_NAME with the name of the binary.
	name, _ := util.NameAndDescription()
	doc = strings.ReplaceAll(doc, "<BINARY_NAME>", name)

	parsedArgs, err := docopt.Parse(doc, args, true, "", false, false)
	if err != nil {
		return fmt.Errorf("Invalid option: 'calicoctl %s'. Use flag '--help' to read about a specific subcommand.", strings.Join(args, " "))
	}
	if len(parsedArgs) == 0 {
		return nil
	}
	ctx := context.Background()

	// Create a new backend client from env vars.
	cf := parsedArgs["--config"].(string)
	client, err := clientmgr.NewClient(cf)
	if err != nil {
		return err
	}

	// Get the backend client.
	type accessor interface {
		Backend() bapi.Client
	}
	bc, ok := client.(accessor).Backend().(*k8s.KubeClient)
	if !ok {
		return fmt.Errorf("IPAM check only supports Kubernetes Datastore Driver")
	}
	kubeClient := bc.ClientSet
	showAllIPs := parsedArgs["--show-all-ips"].(bool)
	showProblemIPs := showAllIPs || parsedArgs["--show-problem-ips"].(bool)
	checker := NewIPAMChecker(kubeClient, client, bc, showAllIPs, showProblemIPs)

	return checker.checkIPAM(ctx)
}

func NewIPAMChecker(k8sClient kubernetes.Interface,
	v3Client clientv3.Interface,
	backendClient bapi.Client,
	showAllIPs bool,
	showProblemIPs bool) *IPAMChecker {
	return &IPAMChecker{
		allocations:       map[string][]allocation{},
		allocationsByNode: map[string][]string{},

		inUseIPs: map[string][]ownerRecord{},

		k8sClient:     k8sClient,
		v3Client:      v3Client,
		backendClient: backendClient,

		showAllIPs:     showAllIPs,
		showProblemIPs: showProblemIPs,
	}
}

type IPAMChecker struct {
	allocations       map[string][]allocation
	allocationsByNode map[string][]string
	inUseIPs          map[string][]ownerRecord

	k8sClient     kubernetes.Interface
	backendClient bapi.Client
	v3Client      clientv3.Interface

	showAllIPs     bool
	showProblemIPs bool
}

func (c *IPAMChecker) checkIPAM(ctx context.Context) error {
	fmt.Println("Checking IPAM for inconsistencies...")
	fmt.Println()

	{
		fmt.Println("Loading all IPAM blocks...")
		blocks, err := c.backendClient.List(ctx, model.BlockListOptions{}, "")
		if err != nil {
			return fmt.Errorf("failed to list IPAM blocks: %w", err)
		}
		fmt.Printf("Found %d IPAM blocks.\n", len(blocks.KVPairs))

		for _, kvp := range blocks.KVPairs {
			b := kvp.Value.(*model.AllocationBlock)
			affinity := "<none>"
			if b.Affinity != nil {
				affinity = *b.Affinity
			}
			fmt.Printf(" IPAM block %s affinity=%s:\n", b.CIDR, affinity)
			for ord, attrIdx := range b.Allocations {
				if attrIdx == nil {
					continue // IP is not allocated
				}
				c.recordAllocation(b, ord)
			}
		}
		fmt.Printf("IPAM blocks record %d allocations.\n", len(c.allocations))
		fmt.Println()
	}
	var activeIPPools []*cnet.IPNet
	{
		fmt.Println("Loading all IPAM pools...")
		ipPools, err := c.v3Client.IPPools().List(ctx, options.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to load IP pools: %w", err)
		}
		for _, p := range ipPools.Items {
			if p.Spec.Disabled {
				continue
			}
			fmt.Printf("  %s\n", p.Spec.CIDR)
			_, cidr, err := cnet.ParseCIDR(p.Spec.CIDR)
			if err != nil {
				return fmt.Errorf("failed to parse IP pool CIDR: %w", err)
			}
			activeIPPools = append(activeIPPools, cidr)
		}
		fmt.Printf("Found %d active IP pools.\n", len(activeIPPools))
		fmt.Println()
	}

	{
		fmt.Println("Loading all nodes.")
		nodes, err := c.v3Client.Nodes().List(ctx, options.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}
		numNodeIPs := 0
		for _, n := range nodes.Items {
			ips, err := getNodeIPs(n)
			if err != nil {
				return err
			}
			for _, ip := range ips {
				c.recordInUseIP(ip, n, fmt.Sprintf("Node(%s)", n.Name))
				numNodeIPs++
			}
		}
		fmt.Printf("Found %d node tunnel IPs.\n", numNodeIPs)
		fmt.Println()
	}

	{
		fmt.Println("Loading all workload endpoints.")
		weps, err := c.v3Client.WorkloadEndpoints().List(ctx, options.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list workload endpoints: %w", err)
		}
		numNodeIPs := 0
		for _, w := range weps.Items {
			ips, err := getWEPIPs(w)
			if err != nil {
				return err
			}
			for _, ip := range ips {
				c.recordInUseIP(ip, w, fmt.Sprintf("Workload(%s/%s)", w.Namespace, w.Name))
				numNodeIPs++
			}
		}
		fmt.Printf("Found %d workload IPs.\n", numNodeIPs)
		fmt.Printf("Workloads and nodes are using %d IPs.\n", len(c.inUseIPs))
		fmt.Println()
	}

	numProblems := 0
	var allocatedButNotInUseIPs []string
	{
		fmt.Printf("Scanning for IPs that are allocated but not actually in use...\n")
		for ip, allocs := range c.allocations {
			if _, ok := c.inUseIPs[ip]; !ok {
				if c.showProblemIPs {
					for _, alloc := range allocs {
						fmt.Printf("  %s leaked; attrs %v\n", ip, alloc.GetAttrString())
					}
				}
				allocatedButNotInUseIPs = append(allocatedButNotInUseIPs, ip)
			}
		}
		numProblems += len(allocatedButNotInUseIPs)
		fmt.Printf("Found %d IPs that are allocated in IPAM but not actually in use.\n", len(allocatedButNotInUseIPs))
	}

	var inUseButNotAllocatedIPs []string
	var nonCalicoIPs []string
	{
		fmt.Printf("Scanning for IPs that are in use by a workload or node but not allocated in IPAM...\n")
		for ip, owners := range c.inUseIPs {
			if c.showProblemIPs && len(owners) > 1 {
				fmt.Printf("  %s has multiple owners.\n", ip)
			}
			if _, ok := c.allocations[ip]; !ok {
				found := false
				parsedIP := net.ParseIP(ip)
				for _, cidr := range activeIPPools {
					if cidr.Contains(parsedIP) {
						found = true
						break
					}
				}
				if !found {
					if c.showProblemIPs {
						for _, owner := range owners {
							fmt.Printf("  %s in use by %v is not in any active IP pool.\n", ip, owner.FriendlyName)
						}
					}
					nonCalicoIPs = append(nonCalicoIPs, ip)
					continue
				}
				if c.showProblemIPs {
					for _, owner := range owners {
						fmt.Printf("  %s in use by %v and in active IPAM pool but has no IPAM allocation.\n", ip, owner.FriendlyName)
					}
				}
				inUseButNotAllocatedIPs = append(inUseButNotAllocatedIPs, ip)
			}
		}
		numProblems += len(nonCalicoIPs)
		numProblems += len(inUseButNotAllocatedIPs)
		fmt.Printf("Found %d in-use IPs that are not in active IP pools.\n", len(nonCalicoIPs))
		fmt.Printf("Found %d in-use IPs that are in active IP pools but have no corresponding IPAM allocation.\n",
			len(inUseButNotAllocatedIPs))
		fmt.Println()
	}

	fmt.Printf("Check complete; found %d problems.\n", numProblems)

	return nil
}

func getWEPIPs(w apiv3.WorkloadEndpoint) ([]string, error) {
	var ips []string
	for _, a := range w.Spec.IPNetworks {
		ip, err := normaliseIP(a)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IP (%s) of workload %s/%s: %w",
				a, w.Namespace, w.Name, err)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

func (c *IPAMChecker) recordAllocation(b *model.AllocationBlock, ord int) {
	ip := b.OrdinalToIP(ord).String()

	alloc := allocation{
		Block:   b,
		Ordinal: ord,
	}
	c.allocations[ip] = append(c.allocations[ip], alloc)

	if c.showAllIPs {
		fmt.Printf("  %s allocated; attrs %s\n", ip, alloc.GetAttrString())
	}

	attrIdx := *b.Allocations[ord]
	if len(b.Attributes) > attrIdx {
		attrs := b.Attributes[attrIdx]
		if attrs.AttrPrimary != nil && *attrs.AttrPrimary == ipam.WindowsReservedHandle {
			c.recordInUseIP(ip, b, "Reserved for Windows")
		}
	}
}

func (c *IPAMChecker) recordInUseIP(ip string, referrer interface{}, friendlyName string) {
	if c.showAllIPs {
		fmt.Printf("  %s belongs to %s\n", ip, friendlyName)
	}

	c.inUseIPs[ip] = append(c.inUseIPs[ip], ownerRecord{
		FriendlyName: friendlyName,
		Resource:     referrer,
	})
}

func getNodeIPs(n apiv3.Node) ([]string, error) {
	var ips []string
	if n.Spec.IPv4VXLANTunnelAddr != "" {
		ip, err := normaliseIP(n.Spec.IPv4VXLANTunnelAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IPv4VXLANTunnelAddr (%s) of node %s: %w",
				n.Spec.IPv4VXLANTunnelAddr, n.Name, err)
		}
		ips = append(ips, ip)
	}
	if n.Spec.Wireguard != nil && n.Spec.Wireguard.InterfaceIPv4Address != "" {
		ip, err := normaliseIP(n.Spec.Wireguard.InterfaceIPv4Address)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Wireguard.InterfaceIPv4Address (%s) of node %s: %w",
				n.Spec.Wireguard.InterfaceIPv4Address, n.Name, err)
		}
		ips = append(ips, ip)
	}
	if n.Spec.BGP != nil && n.Spec.BGP.IPv4IPIPTunnelAddr != "" {
		ip, err := normaliseIP(n.Spec.BGP.IPv4IPIPTunnelAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse IPv4IPIPTunnelAddr (%s) of node %s: %w",
				n.Spec.BGP.IPv4IPIPTunnelAddr, n.Name, err)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

func normaliseIP(addr string) (string, error) {
	ip, _, err := cnet.ParseCIDROrIP(addr)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

type allocation struct {
	Block   *model.AllocationBlock
	Ordinal int
}

func (a allocation) GetAttrString() string {
	attrIdx := *a.Block.Allocations[a.Ordinal]
	if len(a.Block.Attributes) > attrIdx {
		return formatAttrs(a.Block.Attributes[attrIdx])
	}
	return "<missing>"
}

func formatAttrs(attribute model.AllocationAttribute) string {
	primary := "<none>"
	if attribute.AttrPrimary != nil {
		primary = *attribute.AttrPrimary
	}
	var keys []string
	for k := range attribute.AttrSecondary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kvs []string
	for _, k := range keys {
		kvs = append(kvs, fmt.Sprintf("%s=%s", k, attribute.AttrSecondary[k]))
	}
	return fmt.Sprintf("Main:%s Extra:%s", primary, strings.Join(kvs, ","))
}

type ownerRecord struct {
	FriendlyName string
	Resource     interface{}
}
