package api

import (
	"time"

	"davit/internal/domain"
)

// G11 app endpoints (feature appEndpoints): a stable, tailnet-private HTTPS
// URL on the shared gateway for an app listening on a guest loopback port.

// AppEndpointState says whether an endpoint's URL serves right now.
type AppEndpointState string

const (
	// AppEndpointActive: requests reach the app on the VM's current instance.
	AppEndpointActive AppEndpointState = "active"
	// AppEndpointHidden: hidden by the owner; its port is closed and kept.
	AppEndpointHidden AppEndpointState = "hidden"
	// AppEndpointVMNotRunning: the VM is stopped (or starting); requests get
	// 503 until it runs again, at the same URL.
	AppEndpointVMNotRunning AppEndpointState = "vm_not_running"
	// AppEndpointGatewayDown: the tailnet gateway is not up yet.
	AppEndpointGatewayDown AppEndpointState = "gateway_down"
)

// AppEndpointKind is what an endpoint serves.
type AppEndpointKind string

const (
	// AppEndpointHTTP proxies HTTP (and WebSockets) to the app.
	AppEndpointHTTP AppEndpointKind = "http"
	// AppEndpointDesktop serves the built-in desktop viewer and a
	// websockify-compatible RFB stream (/websockify) to the guest's VNC
	// server (G15, openDesktop).
	AppEndpointDesktop AppEndpointKind = "desktop"
)

// AppEndpoint is one app endpoint. It belongs to the VM (not an instance
// generation): stop and resume keep the URL, live connections are cut when
// an instance ends, delete removes it, and a fork does not inherit it.
type AppEndpoint struct {
	ID   string      `json:"id"`
	VMID domain.VMID `json:"vmId"`
	// Name is unique within the VM (DNS label, no "--").
	Name string `json:"name"`
	// Port is the guest loopback port the app listens on.
	Port int `json:"port"`
	// Kind is http, or desktop for the VM's desktop (openDesktop).
	Kind AppEndpointKind `json:"kind"`
	// URL is the stable tailnet URL (https://<gateway>:<gatewayPort>/);
	// empty until the gateway knows its name.
	URL string `json:"url,omitempty"`
	// GatewayPort is the gateway port allocated to this endpoint for life.
	GatewayPort int  `json:"gatewayPort"`
	Hidden      bool `json:"hidden"`
	// ForwardIdentity sends the caller's tailnet login to the app as
	// Tailscale-User-Login. Off by default: the guest is untrusted and
	// learns nothing about who is on the tailnet unless the owner asks.
	ForwardIdentity bool             `json:"forwardIdentity"`
	State           AppEndpointState `json:"state"`
	CreatedAt       time.Time        `json:"createdAt"`
}

// CreateAppEndpointRequest is the body of createAppEndpoint.
type CreateAppEndpointRequest struct {
	Port int `json:"port"`
	// Name defaults to "port-<port>".
	Name string `json:"name,omitempty"`
	// ForwardIdentity: see AppEndpoint.ForwardIdentity (default false).
	ForwardIdentity bool `json:"forwardIdentity,omitempty"`
}

// UpdateAppEndpointRequest is the body of updateAppEndpoint (rename, hide,
// show); absent fields are unchanged.
type UpdateAppEndpointRequest struct {
	Name            *string `json:"name,omitempty"`
	Hidden          *bool   `json:"hidden,omitempty"`
	ForwardIdentity *bool   `json:"forwardIdentity,omitempty"`
}

// AppEndpointList is a VM's app endpoints, oldest first.
type AppEndpointList struct {
	Items []AppEndpoint `json:"items"`
}

// G15 desktop (feature desktop): the VM's VNC desktop as a tailnet-private
// web viewer on the shared gateway. It is the VM's app endpoint of kind
// desktop (named "desktop"), with the same URL, lifecycle and revocation.

// DefaultDesktopPort is the guest VNC port (x11vnc on 127.0.0.1:5900).
const DefaultDesktopPort = 5900

// OpenDesktopRequest is the body of openDesktop.
type OpenDesktopRequest struct {
	// Port is the guest VNC server's loopback port (default 5900).
	Port int `json:"port,omitempty"`
}
