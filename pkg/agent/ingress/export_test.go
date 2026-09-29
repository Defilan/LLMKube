/*
Copyright 2025.

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

package ingress

import "net"

// listenAddr returns the address Start bound, for tests that start the
// server on port 0. Test-only: production code logs the address instead.
func (s *Server) listenAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
