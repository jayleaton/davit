// Package appendpoint manages app endpoints (feature appEndpoints, G11):
// stable, tailnet-private URLs on the shared gateway for apps listening on a
// guest loopback port. It owns the records (a JSON store in the tailnet
// state dir, so URLs survive daemon restarts), gateway port allocation, and
// lifecycle: an endpoint belongs to its VM, so stop keeps the URL (requests
// get 503 meanwhile), every instance change cuts live connections, delete
// removes the endpoints and forks inherit none. The gateway serves the
// routes (gateway.SetApps) and authorizes each request.
package appendpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/gateway"
)

// DefaultPorts is the default gateway port range for app endpoints.
var DefaultPorts = [2]int{10000, 10999}

// reconcileEvery backstops lifecycle notifications.
const reconcileEvery = 2 * time.Second

// VMs resolves VMs in a principal's scope and reports changes.
type VMs interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
	Changed() <-chan struct{}
}

// Gateway serves app routes (gateway.Gateway).
type Gateway interface {
	SetApps([]gateway.AppRoute)
	CutApps(vm domain.VMID, keepGeneration int64)
	AppBase() string
}

// Config configures a Service.
type Config struct {
	// Path is the JSON store.
	Path string
	VMs  VMs
	// Ports is the inclusive gateway port range; zero means DefaultPorts.
	Ports [2]int
	Log   *slog.Logger
}

type record struct {
	ID          string             `json:"id"`
	Owner       domain.PrincipalID `json:"owner"`
	VMID        domain.VMID        `json:"vmId"`
	Name        string             `json:"name"`
	Port        int                `json:"port"`
	GatewayPort int                `json:"gatewayPort"`
	Hidden      bool               `json:"hidden"`
	// Kind is "" (http) or desktop (G15).
	Kind api.AppEndpointKind `json:"kind,omitempty"`
	// ForwardIdentity: the app gets the caller's tailnet login.
	ForwardIdentity bool      `json:"forwardIdentity,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

// seen is the last instance observed for a VM with endpoints.
type seen struct {
	live bool
	gen  int64
}

// Service owns the app endpoints of this host.
type Service struct {
	cfg  Config
	log  *slog.Logger
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu   sync.Mutex
	recs []*record
	gw   Gateway
	last map[domain.VMID]seen
}

// New loads the store and starts the lifecycle watcher.
func New(cfg Config) (*Service, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Ports == ([2]int{}) {
		cfg.Ports = DefaultPorts
	}
	if cfg.Ports[0] < 1 || cfg.Ports[1] > 65535 || cfg.Ports[0] > cfg.Ports[1] {
		return nil, fmt.Errorf("appendpoint: invalid port range %d-%d", cfg.Ports[0], cfg.Ports[1])
	}
	s := &Service{cfg: cfg, log: cfg.Log, last: map[domain.VMID]seen{}}
	if b, err := os.ReadFile(cfg.Path); err == nil {
		if err := json.Unmarshal(b, &s.recs); err != nil {
			return nil, fmt.Errorf("appendpoint: %s: %w", cfg.Path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.wg.Go(func() { s.watch(ctx) })
	return s, nil
}

// SetGateway attaches the tailnet gateway and serves the visible endpoints
// on it. Without a gateway every operation is unsupported_capability.
func (s *Service) SetGateway(gw Gateway) {
	s.mu.Lock()
	s.gw = gw
	s.mu.Unlock()
	s.sync()
}

// Close stops the watcher.
func (s *Service) Close() {
	s.stop()
	s.wg.Wait()
}

func (s *Service) gateway() (Gateway, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gw == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "app endpoints need the tailnet gateway (tailnet.enabled)").
			WithDetail("feature", domain.FeatureAppEndpoints)
	}
	return s.gw, nil
}

// sync pushes the visible routes to the gateway.
func (s *Service) sync() {
	s.mu.Lock()
	gw := s.gw
	var routes []gateway.AppRoute
	for _, r := range s.recs {
		if !r.Hidden {
			routes = append(routes, gateway.AppRoute{ID: r.ID, VMID: r.VMID, GuestPort: uint16(r.Port), GatewayPort: r.GatewayPort,
				ForwardIdentity: r.ForwardIdentity, Desktop: r.Kind == api.AppEndpointDesktop})
		}
	}
	s.mu.Unlock()
	if gw != nil {
		gw.SetApps(routes)
	}
}

// save persists the records; s.mu held.
func (s *Service) save() error {
	b, err := json.MarshalIndent(s.recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.cfg.Path), ".app-endpoints-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.cfg.Path)
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(n string) error {
	if !nameRE.MatchString(n) || strings.Contains(n, "--") {
		return domain.Errorf(domain.CodeInvalidArgument, "name %q must be a DNS label (a-z, 0-9, '-', at most 63) without \"--\"", n).
			WithDetail("field", "name")
	}
	return nil
}

func live(vm *domain.VM) bool {
	return vm.State == domain.StateReady || vm.State == domain.StateRunning
}

// view renders r.
func (s *Service) view(r *record, base string, vmLive bool) api.AppEndpoint {
	out := api.AppEndpoint{ID: r.ID, VMID: r.VMID, Name: r.Name, Port: r.Port, GatewayPort: r.GatewayPort,
		Hidden: r.Hidden, ForwardIdentity: r.ForwardIdentity, State: api.AppEndpointActive, CreatedAt: r.CreatedAt,
		Kind: api.AppEndpointHTTP}
	if r.Kind == api.AppEndpointDesktop {
		out.Kind = api.AppEndpointDesktop
	}
	if base != "" {
		out.URL = base + ":" + strconv.Itoa(r.GatewayPort) + "/"
	}
	switch {
	case r.Hidden:
		out.State = api.AppEndpointHidden
	case base == "":
		out.State = api.AppEndpointGatewayDown
	case !vmLive:
		out.State = api.AppEndpointVMNotRunning
	}
	return out
}

// Create gives ref's guest port an endpoint.
func (s *Service) Create(ctx context.Context, p domain.PrincipalID, ref string, req api.CreateAppEndpointRequest) (api.AppEndpoint, error) {
	return s.create(ctx, p, ref, req, "")
}

// desktopName is the name of a VM's desktop endpoint.
const desktopName = "desktop"

// OpenDesktop returns ref's desktop endpoint, creating it (created true) on
// the guest VNC port (default api.DefaultDesktopPort).
func (s *Service) OpenDesktop(ctx context.Context, p domain.PrincipalID, ref string, port int) (api.AppEndpoint, bool, error) {
	if port == 0 {
		port = api.DefaultDesktopPort
	}
	if ep, err := s.GetDesktop(ctx, p, ref); err == nil {
		if ep.Port != port {
			return api.AppEndpoint{}, false, domain.Errorf(domain.CodeInvalidState,
				"the desktop is open on guest port %d; close it to open another port", ep.Port)
		}
		return ep, false, nil
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return api.AppEndpoint{}, false, err
	}
	ep, err := s.create(ctx, p, ref, api.CreateAppEndpointRequest{Port: port, Name: desktopName}, api.AppEndpointDesktop)
	return ep, err == nil, err
}

// GetDesktop returns ref's desktop endpoint (not_found if none).
func (s *Service) GetDesktop(ctx context.Context, p domain.PrincipalID, ref string) (api.AppEndpoint, error) {
	l, err := s.List(ctx, p, ref)
	if err != nil {
		return api.AppEndpoint{}, err
	}
	for _, ep := range l {
		if ep.Kind == api.AppEndpointDesktop {
			return ep, nil
		}
	}
	return api.AppEndpoint{}, domain.Errorf(domain.CodeNotFound, "vm %s has no open desktop (openDesktop)", ref)
}

// CloseDesktop deletes ref's desktop endpoint, cutting every viewer.
func (s *Service) CloseDesktop(ctx context.Context, p domain.PrincipalID, ref string) error {
	ep, err := s.GetDesktop(ctx, p, ref)
	if err != nil {
		return err
	}
	return s.Delete(ctx, p, ref, ep.ID)
}

func (s *Service) create(ctx context.Context, p domain.PrincipalID, ref string, req api.CreateAppEndpointRequest, kind api.AppEndpointKind) (api.AppEndpoint, error) {
	gw, err := s.gateway()
	if err != nil {
		return api.AppEndpoint{}, err
	}
	if req.Port < 1 || req.Port > 65535 {
		return api.AppEndpoint{}, domain.Errorf(domain.CodeInvalidArgument, "port must be 1..65535").WithDetail("field", "port")
	}
	if req.Name == "" {
		req.Name = "port-" + strconv.Itoa(req.Port)
	}
	if err := validName(req.Name); err != nil {
		return api.AppEndpoint{}, err
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return api.AppEndpoint{}, err
	}
	if vm.State == domain.StateDeleting || vm.State == domain.StateDeleted {
		return api.AppEndpoint{}, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	s.mu.Lock()
	used := map[int]bool{}
	for _, r := range s.recs {
		used[r.GatewayPort] = true
		if r.VMID != vm.ID {
			continue
		}
		if r.Port == req.Port {
			s.mu.Unlock()
			return api.AppEndpoint{}, domain.Errorf(domain.CodeInvalidState, "guest port %d of vm %s already has endpoint %s", req.Port, vm.ID, r.Name).
				WithDetail("endpointId", r.ID)
		}
		if r.Name == req.Name {
			s.mu.Unlock()
			return api.AppEndpoint{}, domain.Errorf(domain.CodeInvalidState, "vm %s already has an endpoint named %s", vm.ID, req.Name).
				WithDetail("endpointId", r.ID)
		}
	}
	port := 0
	for c := s.cfg.Ports[0]; c <= s.cfg.Ports[1]; c++ {
		if !used[c] {
			port = c
			break
		}
	}
	if port == 0 {
		s.mu.Unlock()
		return api.AppEndpoint{}, domain.Errorf(domain.CodeResourceExhausted, "no free gateway port for app endpoints (%d-%d)", s.cfg.Ports[0], s.cfg.Ports[1])
	}
	r := &record{ID: domain.NewID("app"), Owner: p, VMID: vm.ID, Name: req.Name, Port: req.Port, GatewayPort: port,
		ForwardIdentity: req.ForwardIdentity, Kind: kind, CreatedAt: time.Now().UTC()}
	s.recs = append(s.recs, r)
	if _, known := s.last[vm.ID]; !known {
		s.last[vm.ID] = seen{live: live(vm), gen: vm.InstanceGeneration}
	}
	if err := s.save(); err != nil {
		s.recs = s.recs[:len(s.recs)-1]
		s.mu.Unlock()
		return api.AppEndpoint{}, err
	}
	out := s.view(r, gw.AppBase(), live(vm))
	s.mu.Unlock()
	s.sync()
	s.log.Info("app endpoint created", "id", r.ID, "vm", vm.ID, "name", r.Name, "port", r.Port, "gatewayPort", port)
	return out, nil
}

// List returns ref's endpoints, oldest first.
func (s *Service) List(ctx context.Context, p domain.PrincipalID, ref string) ([]api.AppEndpoint, error) {
	gw, err := s.gateway()
	if err != nil {
		return nil, err
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	base := gw.AppBase()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.AppEndpoint{}
	for _, r := range s.recs {
		if r.VMID == vm.ID && r.Owner == p {
			out = append(out, s.view(r, base, live(vm)))
		}
	}
	return out, nil
}

// find resolves ref and endpoint (ID or name) in p's scope; s.mu held.
func (s *Service) find(vm domain.VMID, p domain.PrincipalID, endpoint string) (int, error) {
	for i, r := range s.recs {
		if r.VMID == vm && r.Owner == p && (r.ID == endpoint || r.Name == endpoint) {
			return i, nil
		}
	}
	return -1, domain.Errorf(domain.CodeNotFound, "app endpoint %q not found on vm %s", endpoint, vm)
}

// Update renames, hides or shows an endpoint.
func (s *Service) Update(ctx context.Context, p domain.PrincipalID, ref, endpoint string, req api.UpdateAppEndpointRequest) (api.AppEndpoint, error) {
	gw, err := s.gateway()
	if err != nil {
		return api.AppEndpoint{}, err
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return api.AppEndpoint{}, err
	}
	if req.Name != nil {
		if err := validName(*req.Name); err != nil {
			return api.AppEndpoint{}, err
		}
	}
	s.mu.Lock()
	i, err := s.find(vm.ID, p, endpoint)
	if err != nil {
		s.mu.Unlock()
		return api.AppEndpoint{}, err
	}
	r := s.recs[i]
	if req.Name != nil && *req.Name != r.Name {
		for _, o := range s.recs {
			if o.VMID == vm.ID && o.Name == *req.Name {
				s.mu.Unlock()
				return api.AppEndpoint{}, domain.Errorf(domain.CodeInvalidState, "vm %s already has an endpoint named %s", vm.ID, *req.Name)
			}
		}
	}
	prev := *r
	if req.Name != nil {
		r.Name = *req.Name
	}
	if req.Hidden != nil {
		r.Hidden = *req.Hidden
	}
	if req.ForwardIdentity != nil {
		r.ForwardIdentity = *req.ForwardIdentity
	}
	if err := s.save(); err != nil {
		*r = prev
		s.mu.Unlock()
		return api.AppEndpoint{}, err
	}
	out := s.view(r, gw.AppBase(), live(vm))
	s.mu.Unlock()
	if prev.Hidden != r.Hidden || prev.ForwardIdentity != r.ForwardIdentity {
		// Hiding closes the port; changing identity forwarding re-serves
		// it. Either cuts the endpoint's live connections.
		s.sync()
	}
	return out, nil
}

// Delete removes an endpoint, freeing its gateway port.
func (s *Service) Delete(ctx context.Context, p domain.PrincipalID, ref, endpoint string) error {
	if _, err := s.gateway(); err != nil {
		return err
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return err
	}
	s.mu.Lock()
	i, err := s.find(vm.ID, p, endpoint)
	if err == nil {
		s.recs = slices.Delete(s.recs, i, i+1)
		err = s.save()
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.sync()
	return nil
}

// forgetVM removes vm's endpoints (the VM was deleted).
func (s *Service) forgetVM(vm domain.VMID) {
	s.mu.Lock()
	n := len(s.recs)
	s.recs = slices.DeleteFunc(s.recs, func(r *record) bool { return r.VMID == vm })
	changed := len(s.recs) != n
	if changed {
		if err := s.save(); err != nil {
			s.log.Warn("app endpoints: save", "err", err)
		}
	}
	delete(s.last, vm)
	s.mu.Unlock()
	if changed {
		s.log.Info("app endpoints removed with their vm", "vm", vm)
		s.sync()
	}
}

func (s *Service) watch(ctx context.Context) {
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		changed := s.cfg.VMs.Changed()
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

// reconcile cuts live connections when a VM's instance ends or changes, and
// drops the endpoints of deleted VMs.
func (s *Service) reconcile(ctx context.Context) {
	s.mu.Lock()
	gw := s.gw
	owners := map[domain.VMID]domain.PrincipalID{}
	for _, r := range s.recs {
		owners[r.VMID] = r.Owner
	}
	s.mu.Unlock()
	for vmID, owner := range owners {
		vm, err := s.cfg.VMs.GetVM(ctx, owner, string(vmID))
		if ctx.Err() != nil {
			return
		}
		if err != nil || vm.State == domain.StateDeleting || vm.State == domain.StateDeleted {
			if gw != nil {
				gw.CutApps(vmID, 0)
			}
			s.forgetVM(vmID)
			continue
		}
		now := seen{live: live(vm), gen: vm.InstanceGeneration}
		s.mu.Lock()
		prev, known := s.last[vmID]
		s.last[vmID] = now
		s.mu.Unlock()
		// First sight (e.g. after a daemon restart) cuts anything not on
		// the current instance, to be safe; afterwards only changes do.
		if (known && prev == now) || gw == nil {
			continue
		}
		keep := now.gen
		if !now.live {
			keep = 0
		}
		gw.CutApps(vmID, keep)
	}
}
