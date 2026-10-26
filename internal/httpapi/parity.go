package httpapi

import (
	"net/http"

	"davit/internal/domain"
)

// parityRoute is one parity v1 contract operation: specified in
// openapi.yaml (x-davit-implemented: false), routed with its real access
// rule, and answering a typed not_implemented error that names its feature
// (capabilities.features) until an implementer replaces the handler.
type parityRoute struct {
	pattern   string
	need      access
	feature   string
	operation string
	upload    bool
}

// parityRoutes lists the contract operations still unbuilt: none since
// G18 shipped. A future contract change adds its operations here, e.g.
// r("GET "+api.BasePath+"/x", onVM(api.TokenView), feature, "operationId").
func parityRoutes() []parityRoute { return nil }

// notImplemented answers a contract operation that is not built yet.
func notImplemented(feature, operation string) handler {
	return func(http.ResponseWriter, *http.Request) error {
		return domain.NotImplemented(feature, operation)
	}
}
