package protocol

import "context"

// ControlSender is the capability interface a backend uses to POST a Control
// to the frontend. *backendrpc.Client satisfies it via SendControl. Defined
// here in the contract package (not in backendrpc) so backends can depend on
// the seam without importing the transport — this keeps the per-backend
// handlers free of a backendrpc dependency, and lets a backend
// substitute a test fake that depends only on protocol.
type ControlSender interface {
	SendControl(ctx context.Context, ctrl *Control) error
}

// StatusQuerier is the capability interface a backend uses to read the
// frontend's in-flight turn snapshot via GET /v1/status. *backendrpc.Client
// satisfies it via Status.
type StatusQuerier interface {
	Status(ctx context.Context) (*StatusSnapshot, error)
}
