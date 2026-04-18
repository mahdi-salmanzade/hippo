// Package providers contains the Provider interface contract and the
// concrete provider implementations (one subpackage per backend).
//
// Provider is an alias for hippo.Provider. It lives here so concrete
// provider packages can import a sibling rather than reach up to the root
// module, and so the interface contract is discoverable alongside its
// implementations.
package providers

import "github.com/mahdi-salmanzade/hippo"

// Provider is the contract every LLM backend must satisfy. See the root
// hippo package for the authoritative definition; it is re-exported here
// via type alias for ergonomic access from concrete provider packages.
type Provider = hippo.Provider

// ModelInfo is the re-exported alias for hippo.ModelInfo.
type ModelInfo = hippo.ModelInfo
