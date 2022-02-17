/*
Copyright 2022 Gravitational, Inc.

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

package types

// Ensure services implement ProxiedService.
var (
	_ ProxiedService = &ServerV2{}
	_ ProxiedService = &AppServerV3{}
	_ ProxiedService = &DatabaseServerV3{}
	_ ProxiedService = &WindowsDesktopServiceV3{}
)

// ProxiedService is a service that is connected to a proxy.
type ProxiedService interface {
	// GetProxyIDs returns a list of proxy ids this service is connected to.
	GetProxyIDs() []string
	// SetProxyIDs sets the proxy ids this service is connected to.
	SetProxyIDs([]string)
	// GetNonceID returns the nonce id.
	GetNonceID() uint64
	// SetNonceID sets the nonce id.
	SetNonceID(uint64)
	// GetNonce returns the nonce.
	GetNonce() uint64
	// SetNonce sets the nonce.
	SetNonce(uint64)
}
