//go:build linux

package firecracker

import (
	"strings"

	"davit/internal/runtime"
)

// defaultGuestMAC is used when a network handle names a TAP but no MAC
// (network.DefaultGuestMAC).
const defaultGuestMAC = "06:00:AC:10:00:02"

func (n NIC) layout() string {
	if n.Tap == "" {
		return "none"
	}
	mac := n.MAC
	if mac == "" {
		mac = defaultGuestMAC
	}
	return "nic:" + n.Tap + "/" + strings.ToUpper(mac)
}

// nicOf is the layout a network handle gives the guest.
func nicOf(h runtime.NetworkHandle) NIC { return NIC{Tap: h.TapName, MAC: h.GuestMAC} }

// defaultBootArgs match images/manifests (kernel.bootArgs) and are used when
// an image does not carry its own.
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=1 i8042.noaux i8042.nomux i8042.dumbkbd root=/dev/vda rw"
