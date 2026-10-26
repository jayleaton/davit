package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"

	"davit/api"
	"davit/internal/domain"
)

// TestParityOperationsAnswerNotImplemented ties the contract together: every
// operation openapi.yaml marks x-davit-implemented: false is routed,
// answers 501 not_implemented naming a feature, and that feature lists the
// operation in the capability report. A contract operation can therefore not
// be missing from the router (404) or from capabilities, nor fake success.
func TestParityOperationsAnswerNotImplemented(t *testing.T) {
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	m, err := doc.BuildV3Model()
	if err != nil {
		t.Fatal(err)
	}
	h := newServer(t, fixedAuth{p: "local:1"})
	rec, _ := call(t, h, "GET", "/api/v1/capabilities", "", nil)
	var caps api.HostCapabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	features := map[string]domain.Feature{}
	for _, f := range caps.Features {
		features[f.Name] = f
	}

	param := regexp.MustCompile(`\{[^}]+\}`)
	n := 0
	for path, item := range m.Model.Paths.PathItems.FromOldest() {
		for method, op := range item.GetOperations().FromOldest() {
			if impl, ok := op.Extensions.Get("x-davit-implemented"); !ok || impl.Value != "false" {
				continue
			}
			n++
			url := api.BasePath + param.ReplaceAllString(path, "x1")
			rec, er := call(t, h, strings.ToUpper(method), url, "{}", map[string]string{api.HeaderIdempotencyKey: "k1"})
			if rec.Code != http.StatusNotImplemented || er.Error.Code != domain.CodeNotImplemented {
				t.Errorf("%s %s (%s): got %d %+v, want 501 not_implemented", method, path, op.OperationId, rec.Code, er.Error)
				continue
			}
			name, _ := er.Error.Details["feature"].(string)
			f, ok := features[name]
			if !ok || f.Status != domain.FeatureNotImplemented || !slices.Contains(f.Operations, op.OperationId) {
				t.Errorf("%s: error names feature %q, which capabilities do not list as not_implemented with this operation", op.OperationId, name)
			}
			if got, _ := er.Error.Details["operation"].(string); got != op.OperationId {
				t.Errorf("%s: error names operation %q", op.OperationId, got)
			}
		}
	}
	if n != 0 { // the parity 30, the G9, G16 and G18 operations shipped
		t.Errorf("%d contract operations, want 0 (update this count with the contract)", n)
	}
}

// TestContractFieldsRefused: new fields on implemented operations are
// refused with not_implemented, never silently ignored.
func TestContractFieldsRefused(t *testing.T) {
	h := newServer(t, fixedAuth{p: "local:1"})
	env := `{"environment":{"environment":"web"}}`
	for _, c := range []struct{ method, path, body, op, feature string }{
		{"POST", "/api/v1/vms", `{"name":"a","environment":{"environment":"web"}}`, "createVM.environment", domain.FeatureWorkspacePrep},
		{"POST", "/api/v1/vms/a/fork", env, "forkVM.environment", domain.FeatureWorkspacePrep},
		{"POST", "/api/v1/vms/a/resume", env, "resumeVM.environment", domain.FeatureWorkspacePrep},
	} {
		rec, er := call(t, h, c.method, c.path, c.body, map[string]string{api.HeaderIdempotencyKey: "k-" + c.op})
		if rec.Code != http.StatusNotImplemented || er.Error.Details["operation"] != c.op ||
			er.Error.Details["feature"] != c.feature {
			t.Errorf("%s: got %d %+v", c.op, rec.Code, er.Error)
		}
	}
}
