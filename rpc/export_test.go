package rpc

// Internals this package's external cells need, and nothing else.
//
// IT IS HERE RATHER THAN ON THE PUBLIC Option SET because it is a test seam,
// and an option that ships is an option somebody can reach for. export_test.go
// is compiled into this package only under `go test`, so the shipped surface is
// unchanged.

// SetShutdownAuditGate installs an interceptor around sys.shutdown's audit
// write, so a cell can hold one decision inside its ownership window while
// another arrives, or make the write fail and drive the abort that follows.
func SetShutdownAuditGate(s *Server, fn func(next func() error) error) {
	s.hookShutdownAudit = fn
}
