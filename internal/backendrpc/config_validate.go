package backendrpc

import "fmt"

// ValidateBackendConfig checks the three fields every backend needs to
// register with the frontend IPC: a shared bearer secret, a unique
// backend id, and the frontend URL to dial. Failing here at startup
// yields a clear error instead of an opaque registration failure or a
// dial that hangs until systemd's TimeoutStartSec.
//
// The frontend (feishu-front) is the IPC server, not a client, so it does
// not call this — it only needs IPCSecret and validates that inline.
func ValidateBackendConfig(ipcSecret, backendID, frontendURL string) error {
	if ipcSecret == "" {
		return fmt.Errorf("ipc_secret is required (frontend IPC rejects connections without it)")
	}
	if backendID == "" {
		return fmt.Errorf("backend_id is required")
	}
	if frontendURL == "" {
		return fmt.Errorf("frontend_url is required")
	}
	return nil
}
