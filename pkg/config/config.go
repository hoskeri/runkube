package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/runkube/pkg/randid"
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
	// LogLevels is the default log level for control-plane components and
	// nodes plus per-tag overrides. Overrides name a process tag (etcd, api,
	// controller, scheduler, webhook, kms, jwtsigner), a node name, or an
	// in-process component as either its full path (webhook/validate) or its
	// component name alone (validate).
	LogLevels process.LogLevels `json:"logLevels"`
	// HealthGrace is how long a critical component (etcd, the apiserver) may
	// stay unhealthy before the control plane tears the formation down.
	// Anything shorter risks killing a cluster over a momentary probe failure.
	HealthGrace time.Duration `json:"healthGrace"`
}

// Parse parses the command line flags and returns the configuration.
func Parse() (Config, []string) {
	fs := pflag.NewFlagSet("runkube", pflag.ExitOnError)
	fs.SortFlags = false

	var c Config
	var serviceAddressRange string
	var logLevels string

	fs.StringVar(&c.Name, "name", "default", "cluster name")
	fs.StringVar(&c.Root, "root", "", "root directory (defaults to ${XDG_STATE_HOME}/runkube/NAME or ~/.local/state/runkube/NAME)")
	fs.StringVar(&c.ExternalHostname, "external-hostname", "", "external hostname (defaults to NAME.runkube.local)")
	fs.StringVar(&serviceAddressRange, "service-address-range", "", "service address range (defaults to a /64 ULA derived from the cluster name)")
	fs.StringSliceVar(&c.Nodes, "node", []string{"node-a"}, "node names (can be specified multiple times)")
	fs.StringVar(&logLevels, "log-level", "error", "a bare log level, or tag=level overrides")
	fs.DurationVar(&c.HealthGrace, "health-grace", 5*time.Minute, "how long a critical component may be unhealthy before the control plane gives up")

	fs.Parse(os.Args[1:])

	parsedLevels, err := process.ParseLogLevels(logLevels, slog.LevelError)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid log-level %q: %v\n", logLevels, err)
		os.Exit(1)
	}
	c.LogLevels = parsedLevels
	if serviceAddressRange == "" {
		c.ServiceAddressRange = randid.ULA(c.Name)
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

	if c.HealthGrace <= 0 {
		return fmt.Errorf("invalid health-grace: must be positive")
	}

	if c.ExternalHostname == "" {
		c.ExternalHostname = fmt.Sprintf("%s.runkube.local", c.Name)
	}

	if !c.ServiceAddressRange.IsValid() {
		return fmt.Errorf("invalid service-address-range")
	}

	return nil
}
