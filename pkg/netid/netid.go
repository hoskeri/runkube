// Package netid derives the emulated network identity of a sandbox or node
// from its machine id: a MAC address, a private IPv4 address and a ULA IPv6
// address. The derivation is deterministic — the same machine id always
// yields the same identity — so identities are predictable across restarts
// without being hardcoded (docs/TODO.md, "Dynamic network interface").
package netid

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
)

// Identity is the emulated L2/L3 identity of a sandbox's default interface,
// derived deterministically from a machine id.
type Identity struct {
	// MAC is the 6-byte interface address in 02:xx:xx:xx:xx:xx form: bit 0
	// (unicast) is cleared and bit 1 (locally administered) is set, so the
	// address can never collide with a real vendor OUI.
	MAC net.HardwareAddr
	// IPv4 is a private 10/8 address: 10.<b12>.<b13>.<b14> from the machine
	// id bytes.
	IPv4 net.IP
	// IPv6 is a ULA (fd00::/8) interface address built from the machine id.
	IPv6 net.IP
}

// FromMachineID derives the identity for the given machine id. A machine id
// that is exactly 16 bytes (32 hex characters) is used as-is; any other
// value is hashed (SHA-256) so every input yields a stable 16-byte identity.
func FromMachineID(machineID string) Identity {
	b := machineIDBytes(machineID)

	var mac net.HardwareAddr = append([]byte(nil), b[0:6]...)
	mac[0] = mac[0]&0xfe | 0x02 // unicast + locally administered

	ip6 := make(net.IP, net.IPv6len)
	ip6[0], ip6[1] = 0xfd, 0x00 // ULA fd00::/8
	copy(ip6[2:], b[0:14])

	return Identity{
		MAC:  mac,
		IPv4: net.IPv4(10, b[12], b[13], b[14]),
		IPv6: ip6,
	}
}

// machineIDBytes returns the 16-byte identity for a machine id string: the
// machine id itself when it is 16 bytes (32 hex characters), otherwise a
// SHA-256 digest truncated to 16 bytes.
func machineIDBytes(machineID string) [16]byte {
	var b [16]byte
	if len(machineID) == 2*16 {
		if raw, err := hex.DecodeString(machineID); err == nil {
			copy(b[:], raw)
			return b
		}
	}
	h := sha256.Sum256([]byte(machineID))
	copy(b[:], h[:16])
	return b
}
