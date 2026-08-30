package randid

import (
	"bytes"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

// refSeed is the machine-id-style seed used across the codebase's sandbox
// examples: exactly 16 bytes, so it is interpreted as raw bytes.
const refSeed = "32e825d5701d11f08f966c02e06b743e"

// TestKnownValues pins the derivation for the reference seed so a regression
// in any byte position is caught. The seed decodes as bytes, so the values
// are readable, not opaque hashes.
func TestKnownValues(t *testing.T) {
	if got := MAC(refSeed).String(); got != "32:e8:25:d5:70:1d" {
		t.Errorf("MAC = %s", got)
	}
	if got := IPv4(refSeed).String(); got != "10.224.107.116" {
		t.Errorf("IPv4 = %s", got)
	}
	if got := IPv6(refSeed).String(); got != "fd00:32e8:25d5:701d:11f0:8f96:6c02:e06b" {
		t.Errorf("IPv6 = %s", got)
	}
	if got := Hex(refSeed, 32); got != refSeed {
		t.Errorf("Hex(seed, 32) = %q, want the seed itself", got)
	}
}

// seedStrings returns representative seeds: a machine-id-style seed, plain
// names, and the empty string.
func seedStrings() []string {
	return []string{refSeed, "node-a", "node-b", "default", ""}
}

func TestDeterministic(t *testing.T) {
	for _, seed := range seedStrings() {
		if !bytes.Equal(MAC(seed), MAC(seed)) {
			t.Errorf("MAC(%q) not deterministic", seed)
		}
		if !IPv4(seed).Equal(IPv4(seed)) {
			t.Errorf("IPv4(%q) not deterministic", seed)
		}
		if !IPv6(seed).Equal(IPv6(seed)) {
			t.Errorf("IPv6(%q) not deterministic", seed)
		}
		if !bytes.Equal(Bytes(seed, 17), Bytes(seed, 17)) {
			t.Errorf("Bytes(%q, 17) not deterministic", seed)
		}
		if Hex(seed, 12) != Hex(seed, 12) {
			t.Errorf("Hex(%q, 12) not deterministic", seed)
		}
	}
}

func TestDistinct(t *testing.T) {
	a, b := refSeed, "11111111111111111111111111111111"
	all := []net.IP{IPv4(a), IPv4(b), IPv6(a), IPv6(b)}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i].Equal(all[j]) {
				t.Errorf("identities collide: %v == %v", all[i], all[j])
			}
		}
	}
	if bytes.Equal(MAC(a), MAC(b)) {
		t.Errorf("MACs collide: %v", MAC(a))
	}
	if ULA("node-a").Addr() == ULA("node-b").Addr() {
		t.Errorf("ULAs collide")
	}
}

func TestMACHasLocalAdminBit(t *testing.T) {
	for _, seed := range seedStrings() {
		mac := MAC(seed)
		if len(mac) != 6 {
			t.Errorf("MAC(%q): want 6 bytes, got %d", seed, len(mac))
		}
		if mac[0]&0x01 != 0 {
			t.Errorf("MAC(%q): multicast bit set", seed)
		}
		if mac[0]&0x02 == 0 {
			t.Errorf("MAC(%q): locally administered bit clear", seed)
		}
	}
}

func TestAddressesInPrivateRanges(t *testing.T) {
	for _, seed := range seedStrings() {
		if ip := IPv4(seed); !ip.IsPrivate() || ip.To4()[0] != 10 {
			t.Errorf("IPv4(%q) %v not a private 10/8 address", seed, ip)
		}
		if ip := IPv6(seed); !ip.IsGlobalUnicast() || ip[0] != 0xfd {
			t.Errorf("IPv6(%q) %v not a fd00::/8 ULA", seed, ip)
		}
	}
}

// TestValuesDerivedFromSameIdentity pins the invariant that every generator
// is a function of the seed's 16 identity bytes, so values stay coherent.
func TestValuesDerivedFromSameIdentity(t *testing.T) {
	for _, seed := range seedStrings() {
		b := Bytes(seed, 16)
		mac := MAC(seed)
		if !bytes.Equal(mac[1:], b[1:6]) || mac[0]&0xfe != b[0]&0xfe {
			t.Errorf("MAC(%q) does not derive from identity bytes", seed)
		}
		ip4 := IPv4(seed).To4()
		if !bytes.Equal(ip4[1:4], b[12:15]) {
			t.Errorf("IPv4(%q) does not derive from identity bytes", seed)
		}
		ip6 := IPv6(seed).To16()
		if !bytes.Equal(ip6[2:], b[:14]) {
			t.Errorf("IPv6(%q) does not derive from identity bytes", seed)
		}
	}
}

func TestBytes(t *testing.T) {
	for _, seed := range seedStrings() {
		if got := Bytes(seed, 0); got != nil {
			t.Errorf("Bytes(%q, 0) = %v, want nil", seed, got)
		}
		for _, n := range []int{1, 6, 16, 17, 32, 64} {
			b := Bytes(seed, n)
			if len(b) != n {
				t.Errorf("Bytes(%q, %d): len = %d", seed, n, len(b))
			}
		}
		// Bytes(seed, 16) must prefix any longer output, so callers can ask
		// for more material without re-deriving the base.
		short, long := Bytes(seed, 16), Bytes(seed, 24)
		if !bytes.Equal(short, long[:16]) {
			t.Errorf("Bytes(%q, 16) not a prefix of Bytes(%q, 24)", seed, seed)
		}
	}
}

func TestHex(t *testing.T) {
	hexRe := regexp.MustCompile(`^[0-9a-f]+$`)
	for _, seed := range seedStrings() {
		for _, n := range []int{1, 2, 5, 12, 32, 40} {
			h := Hex(seed, n)
			if len(h) != n || !hexRe.MatchString(h) {
				t.Errorf("Hex(%q, %d) = %q, want %d lowercase hex chars", seed, n, h, n)
			}
		}
	}
	if Hex("node-a", 4) == Hex("node-b", 4) {
		t.Errorf("expected distinct hex for distinct seeds")
	}
}

func TestULA(t *testing.T) {
	for _, seed := range seedStrings() {
		p := ULA(seed)
		if !p.IsValid() || p.Bits() != 64 {
			t.Errorf("ULA(%q) = %v, want valid /64", seed, p)
		}
		addr := p.Addr().As16()
		if addr[0] != 0xfd {
			t.Errorf("ULA(%q) first byte %#x, want 0xfd", seed, addr[0])
		}
		if !p.Addr().IsGlobalUnicast() {
			t.Errorf("ULA(%q) %v not a unique-local address", seed, p)
		}
	}
	// ULA layout: fd + 5-byte global id + 2-byte subnet id from the first 7
	// identity bytes, /64 — the same shape pkg/config's service address range
	// relies on.
	p := ULA(refSeed)
	want := netip.MustParsePrefix("fd32:e825:d570:1d11::/64")
	if p != want {
		t.Errorf("ULA(%q) = %v, want %v", refSeed, p, want)
	}
}

// TestHashedFallback covers non-hex seeds (arbitrary length or invalid hex):
// they must still yield stable, valid values.
func TestHashedFallback(t *testing.T) {
	if !IPv4("node-a").Equal(IPv4("node-a")) {
		t.Errorf("hashed IPv4 not deterministic")
	}
	if IPv4("node-a").Equal(IPv4("node-b")) {
		t.Errorf("different seeds hash to the same IPv4")
	}
	if !IPv6("0102").IsGlobalUnicast() {
		t.Errorf("short non-hex seed produced a bad IPv6")
	}
	// 32 chars of invalid hex must fall back to hashing, not be misread as
	// raw identity bytes.
	if IPv4(strings.Repeat("z", 32)).Equal(IPv4(strings.Repeat("g", 32))) {
		t.Errorf("invalid-hex seeds should hash distinctly")
	}
}
