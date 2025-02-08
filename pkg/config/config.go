package config

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"

	"github.com/spf13/pflag"
)

var dnsLabelRegexp = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Config holds the configuration for the control plane.
type Config struct {
	Name                string       `json:"name"`
	Root                string       `json:"root"`
	ExternalHostname    string       `json:"externalHostname"`
	ServiceAddressRange netip.Prefix `json:"serviceAddressRange"`
	Nodes               []string     `json:"nodes"`
}

func generateULA(seed string) netip.Prefix {
	hash := sha256.Sum256([]byte(seed))
	var addr [16]byte
	addr[0] = 0xfd
	copy(addr[1:6], hash[0:5])
	copy(addr[6:8], hash[5:7])
	return netip.PrefixFrom(netip.AddrFrom16(addr), 64)
}

// Parse parses the command line flags and returns the configuration.
func Parse() (Config, []string) {
	fs := pflag.NewFlagSet("runkube", pflag.ExitOnError)
	fs.SortFlags = false

	var c Config
	var serviceAddressRange string

	fs.StringVar(&c.Name, "name", "default", "cluster name")
	fs.StringVar(&c.Root, "root", "", "root directory (defaults to ${XDG_STATE_HOME}/runkube/NAME or ~/.local/state/runkube/NAME)")
	fs.StringVar(&c.ExternalHostname, "external-hostname", "", "external hostname (defaults to NAME.runkube.local)")
	fs.StringVar(&serviceAddressRange, "service-address-range", "", "service address range (defaults to a random /64 ULA based on cluster name)")
	fs.StringSliceVar(&c.Nodes, "node", []string{"node-a"}, "node names (can be specified multiple times)")

	fs.Parse(os.Args[1:])

	if serviceAddressRange == "" {
		c.ServiceAddressRange = generateULA(c.Name)
	} else {
		var err error
		c.ServiceAddressRange, err = netip.ParsePrefix(serviceAddressRange)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid service-address-range: %v\n", err)
			os.Exit(1)
		}
	}

	if err := c.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	return c, fs.Args()
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if !dnsLabelRegexp.MatchString(c.Name) {
		return fmt.Errorf("invalid name %q: must be a valid DNS label (lowercase alphanumeric and hyphens, max 63 chars, cannot start or end with hyphen)", c.Name)
	}

	if c.Root == "" {
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			userHomeDir, _ := os.UserHomeDir()
			stateHome = filepath.Join(userHomeDir, ".local", "state")
		}
		c.Root = filepath.Join(stateHome, "runkube", c.Name)
	}

	if c.ExternalHostname == "" {
		c.ExternalHostname = fmt.Sprintf("%s.runkube.local", c.Name)
	}

	if !c.ServiceAddressRange.IsValid() {
		return fmt.Errorf("invalid service-address-range")
	}

	return nil
}
