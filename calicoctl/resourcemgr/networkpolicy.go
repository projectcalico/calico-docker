// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.

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

package resourcemgr

import (
	"context"
	"strings"

	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s/conversion"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/options"
)

func init() {
	registerResource(
		api.NewNetworkPolicy(),
		api.NewNetworkPolicyList(),
		true,
		[]string{"networkpolicy", "networkpolicies", "policy", "np", "policies", "pol", "pols"},
		[]string{"NAME"},
		[]string{"NAME", "ORDER", "SELECTOR"},
		// NAMESPACE may be prepended in GrabTableTemplate so needs to remain in the map below
		map[string]string{
			"NAME":      "{{.ObjectMeta.Name}}",
			"NAMESPACE": "{{.ObjectMeta.Namespace}}",
			"ORDER":     "{{.Spec.Order}}",
			"SELECTOR":  "{{.Spec.Selector}}",
		},
		func(ctx context.Context, client client.Interface, resource ResourceObject) (ResourceObject, error) {
			r := resource.(*api.NetworkPolicy)
			if strings.HasPrefix(r.Name, conversion.K8sNetworkPolicyNamePrefix) {
				return nil, cerrors.ErrorOperationNotSupported{
					Operation:  "create or apply",
					Identifier: resource,
					Reason:     "kubernetes network policies must be managed through the kubernetes API",
				}
			}
			return client.NetworkPolicies().Create(ctx, r, options.SetOptions{})
		},
		func(ctx context.Context, client client.Interface, resource ResourceObject) (ResourceObject, error) {
			r := resource.(*api.NetworkPolicy)
			if strings.HasPrefix(r.Name, conversion.K8sNetworkPolicyNamePrefix) {
				return nil, cerrors.ErrorOperationNotSupported{
					Operation:  "apply or replace",
					Identifier: resource,
					Reason:     "kubernetes network policies must be managed through the kubernetes API",
				}
			}
			return client.NetworkPolicies().Update(ctx, r, options.SetOptions{})
		},
		func(ctx context.Context, client client.Interface, resource ResourceObject) (ResourceObject, error) {
			r := resource.(*api.NetworkPolicy)
			if strings.HasPrefix(r.Name, conversion.K8sNetworkPolicyNamePrefix) {
				return nil, cerrors.ErrorOperationNotSupported{
					Operation:  "delete",
					Identifier: resource,
					Reason:     "kubernetes network policies must be managed through the kubernetes API",
				}
			}
			return client.NetworkPolicies().Delete(ctx, r.Namespace, r.Name, options.DeleteOptions{ResourceVersion: r.ResourceVersion})
		},
		func(ctx context.Context, client client.Interface, resource ResourceObject) (ResourceObject, error) {
			r := resource.(*api.NetworkPolicy)
			if strings.HasPrefix(r.Name, conversion.K8sNetworkPolicyNamePrefix) {
				return nil, cerrors.ErrorOperationNotSupported{
					Operation:  "get",
					Identifier: resource,
					Reason:     "kubernetes network policies must be managed through the kubernetes API",
				}
			}
			return client.NetworkPolicies().Get(ctx, r.Namespace, r.Name, options.GetOptions{ResourceVersion: r.ResourceVersion})
		},
		func(ctx context.Context, client client.Interface, resource ResourceObject) (ResourceListObject, error) {
			r := resource.(*api.NetworkPolicy)
			l, err := client.NetworkPolicies().List(ctx, options.ListOptions{ResourceVersion: r.ResourceVersion, Namespace: r.Namespace, Name: r.Name})
			if err != nil {
				return nil, err
			}

			// Filter out Kubernetes policies. These are managed through kubectl.
			items := []api.NetworkPolicy{}
			for _, v := range l.Items {
				if !strings.HasPrefix(v.Name, conversion.K8sNetworkPolicyNamePrefix) {
					items = append(items, v)
				}
			}
			l.Items = items
			return l, nil
		},
	)
}
