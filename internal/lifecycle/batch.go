package lifecycle

import (
	"context"
	"fmt"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// CreateVMs accepts body.Count VMs from one template (G9, feature
// batchCreate). The template is validated once: an invalid one fails the
// whole request and creates nothing. Each item then goes through Create on
// its own, idempotent on the batch key plus "/" and its 1-based index, so a
// replayed batch returns the same operations and a retry after a partial
// failure creates only the items that are still missing. Items are admitted
// independently: one refused by admission or a name conflict reports its
// error and the others continue. Every item is reported, in order.
//
// A template name N names the items N-1 ... N-Count; without one the items
// are unnamed. Reusing the key with a different template conflicts per item
// (idempotency_conflict); a different count replays the overlapping items
// and creates the rest.
func (s *Service) CreateVMs(ctx context.Context, req Request, body api.CreateVMsRequest) (*api.CreateVMsResult, error) {
	if req.Principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if req.IdempotencyKey == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is required for this request")
	}
	suffix := len("/" + strconv.Itoa(api.MaxBatchCount))
	if len(req.IdempotencyKey) > maxIdempotencyKey-suffix {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key of a batch is at most %d bytes", maxIdempotencyKey-suffix)
	}
	if body.Count < 1 || body.Count > api.MaxBatchCount {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "count must be 1..%d", api.MaxBatchCount).WithDetail("field", "count")
	}
	if body.Template.Environment != nil {
		return nil, domain.NotImplemented(domain.FeatureWorkspacePrep, "createVM.environment")
	}
	names := make([]string, body.Count)
	if body.Template.Name != "" {
		for i := range names {
			names[i] = fmt.Sprintf("%s-%d", body.Template.Name, i+1)
			if !domain.ValidName(names[i]) {
				return nil, domain.Errorf(domain.CodeInvalidArgument, "invalid name %q for item %d: use 1-63 lowercase letters, digits and '-'", names[i], i+1).
					WithDetail("field", "template.name")
			}
		}
	}
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		_, err := s.planCreate(ctx, tx, req.Principal, body.Template)
		return err
	}); err != nil {
		return nil, err
	}

	out := &api.CreateVMsResult{Items: make([]api.BatchItem, 0, body.Count)}
	for i, name := range names {
		item := api.BatchItem{Index: i + 1, Name: name}
		ireq := req
		ireq.IdempotencyKey = req.IdempotencyKey + "/" + strconv.Itoa(i+1)
		tmpl := body.Template
		tmpl.Name = name
		acc, err := s.Create(ctx, ireq, tmpl)
		if err != nil {
			de := domain.AsError(err)
			if de.Code == domain.CodeInternal {
				s.cfg.Log.Error("batch create item", "index", i+1, "err", err)
			}
			item.Error = &api.Error{Code: de.Code, Message: de.Message, Retryable: de.Retryable,
				Details: de.Details, OperationID: de.OperationID}
		} else {
			item.Accepted = &api.OperationAccepted{Operation: acc.Operation, VM: acc.VM}
			if acc.VM != nil {
				item.Name = acc.VM.Name
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
