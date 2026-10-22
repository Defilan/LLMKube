/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"fmt"

	discoveryv1 "k8s.io/api/discovery/v1"
)

// ReadyEndpointURLs returns one "http://<address>:<port>" per address of every
// endpoint the slices do not explicitly mark unready, in slice order. A nil
// ready condition counts as ready, per the EndpointSlice API. The controller
// (status, drain) and the proxy (dispatch) share this rule so they agree on
// which pods are serving.
func ReadyEndpointURLs(slices []*discoveryv1.EndpointSlice, port int32) []string {
	var urls []string
	for _, s := range slices {
		for j := range s.Endpoints {
			ep := &s.Endpoints[j]
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				urls = append(urls, fmt.Sprintf("http://%s:%d", addr, port))
			}
		}
	}
	return urls
}
