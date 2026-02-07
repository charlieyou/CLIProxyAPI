// init.go
package quota

import "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"

// InitQuotaProviders is a marker function that provides an explicit call site
// for ensuring quota providers are registered. The function itself is a no-op;
// actual provider registration occurs via init() functions when this package
// is imported.
//
// Important: This function does NOT register providers on its own. You must
// still import this quota package for registration to occur. All providers
// (Claude, Codex, Gemini, HTTP) are defined within this package and register
// via init() functions when the package is imported.
//
// Primary pattern (preferred): Use blank import in main.go
//
//	import _ "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth/quota"
//
// Fallback pattern (explicit call site for readability):
//
//	import "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth/quota"
//	quota.InitQuotaProviders() // Documents intent; actual registration via import
func InitQuotaProviders() {
	// No-op: importing this package already triggers all init() functions.
	// This function exists solely to provide an explicit call site that
	// documents the intent to initialize quota providers.
}

// RegisteredProviders returns a list of all registered provider keys.
// Useful for startup validation and debugging.
func RegisteredProviders() []string {
	return auth.ListQuotaProviders()
}
