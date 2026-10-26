package network

import "strconv"

// Rootless egress backends translate guest packets into host sockets, so
// they run as the unprivileged user in the host network namespace. Both
// attach a second interface (Config.Uplink) to the VM netns; the guest
// ruleset forwards tap0 -> uplink and masquerades the guest address.
//
// What the backends guarantee on their own (without the in-netns ruleset):
//   - pasta --no-map-gw: the netns gateway address is not mapped to the
//     host's loopback; -T/-U none: no forwarding to host loopback ports;
//     -t/-u none: no inbound port forwarding into the netns.
//   - slirp4netns --disable-host-loopback: 10.0.2.2 does not reach the
//     host's 127.0.0.1.
//
// Neither blocks the host's LAN addresses, the LAN itself, tailnet CGNAT
// addresses or cloud metadata: a socket connect() from the host reaches them.
// That is why the in-netns nftables ruleset carries the deny list.

// PastaArgv builds the pasta command that attaches egress to a rootless VM
// netns. The flags are from passt(1) (pasta defaults -t/-u/-T/-U to auto,
// so "none" must be explicit); pasta daemonizes once the
// namespace is configured and writes its PID to pidFile, which is the
// readiness signal. pasta exits by itself when the namespace goes away.
// Guest traffic is IPv4 only, so pasta is limited to IPv4.
func PastaArgv(cfg Config, userNSPath, netNSPath, pidFile string) []string {
	return []string{
		"pasta",
		"--quiet",
		"--config-net",
		"--ipv4-only",
		"--no-map-gw",
		"--tcp-ports", "none",
		"--udp-ports", "none",
		"--tcp-ns", "none",
		"--udp-ns", "none",
		"--ns-ifname", cfg.Uplink,
		"--pid", pidFile,
		"--userns", userNSPath,
		"--netns", netNSPath,
	}
}

// Slirp4netnsArgv builds the slirp4netns command for a rootless VM netns.
// slirp4netns stays in the foreground; it writes "1" to readyFD once the
// interface is configured. The default slirp network is 10.0.2.0/24
// (gateway 10.0.2.2, DNS 10.0.2.3), which the default deny list (10/8)
// blocks for the guest: the guest must use public resolvers.
// --enable-sandbox/--enable-seccomp are deliberately not passed: sandbox mode
// fails when /etc/resolv.conf links outside /etc or /run, and neither flag
// could be verified on the development host (slirp4netns not installed).
func Slirp4netnsArgv(userNSPath, netNSPath, uplink string, readyFD int) []string {
	return []string{
		"slirp4netns",
		"--configure",
		"--mtu=65520",
		"--disable-host-loopback",
		"--ready-fd=" + strconv.Itoa(readyFD),
		"--netns-type=path",
		"--userns-path=" + userNSPath,
		netNSPath,
		uplink,
	}
}
