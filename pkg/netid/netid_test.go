package netid

import (
	"bytes"
	"net"
	"testing"
)

// The reference machine id used across the codebase's sandbox examples.
const refMachineID = "32e825d5701d11f08f966c02e06b743e"

func TestFromMachineIDDeterministic(t *testing.T) {
	a, b := FromMachineID(refMachineID), FromMachineID(refMachineID)
	if !bytes.Equal(a.MAC, b.MAC) || !a.IPv4.Equal(b.IPv4) || !a.IPv6.Equal(b.IPv6) {
		t.Errorf("identity not deterministic:\n%+v\n%+v", a, b)
	}
}

func TestFromMachineIDDistinct(t *testing.T) {
	a := FromMachineID(refMachineID)
	b := FromMachineID("11111111111111111111111111111111")
	all := []net.IP{a.IPv4, b.IPv4, a.IPv6, b.IPv6}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i].Equal(all[j]) {
				t.Errorf("identities collide: %v == %v", all[i], all[j])
			}
		}
	}
	if bytes.Equal(a.MAC, b.MAC) {
		t.Errorf("macs collide: %v", a.MAC)
	}
}

// TestFromMachineIDKnownPins the derivation for a 32-hex-char machine id:
// the identity is exactly the machine id bytes, so the values are readable,
// not opaque hashes.
func TestFromMachineIDKnown(t *testing.T) {
	id := FromMachineID(refMachineID)
	if got := id.MAC.String(); got != "32:e8:25:d5:70:1d" {
		t.Errorf("mac = %s", got)
	}
	if got := id.IPv4.String(); got != "10.224.107.116" {
		t.Errorf("ipv4 = %s", got)
	}
	if got := id.IPv6.String(); got != "fd00:32e8:25d5:701d:11f0:8f96:6c02:e06b" {
		t.Errorf("ipv6 = %s", got)
	}
}

func TestMACHasLocalAdminBit(t *testing.T) {
	for _, mid := range []string{refMachineID, "node-a", "node-b", ""} {
		mac := FromMachineID(mid).MAC
		if len(mac) != 6 {
			t.Errorf("mac %v: want 6 bytes", mac)
		}
		if mac[0]&0x01 != 0 {
			t.Errorf("mac %v: multicast bit set", mac)
		}
		if mac[0]&0x02 == 0 {
			t.Errorf("mac %v: locally administered bit clear", mac)
		}
	}
}

func TestAddressesInPrivateRanges(t *testing.T) {
	for _, mid := range []string{refMachineID, "node-a", "another-node", "sandbox-uid-1-150405.000000"} {
		id := FromMachineID(mid)
		if !id.IPv4.IsPrivate() {
			t.Errorf("ipv4 %v not private", id.IPv4)
		}
		if !id.IPv6.IsGlobalUnicast() || id.IPv6[0] != 0xfd {
			t.Errorf("ipv6 %v not a ULA", id.IPv6)
		}
	}
}

// TestHashedFallback covers non-hex machine ids (arbitrary length or invalid
// hex): they must still yield a stable, valid identity.
func TestHashedFallback(t *testing.T) {
	a := FromMachineID("node-a")
	b := FromMachineID("node-a")
	if !a.IPv4.Equal(b.IPv4) || !bytes.Equal(a.MAC, b.MAC) || !a.IPv6.Equal(b.IPv6) {
		t.Errorf("hashed identity not deterministic: %+v %+v", a, b)
	}
	if FromMachineID("node-a").IPv4.Equal(FromMachineID("node-b").IPv4) {
		t.Errorf("different machine ids hash to the same ipv4")
	}
}
