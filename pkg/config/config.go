package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	// LogLevel is the default log level for control-plane components and
	// nodes, applied to any component without an explicit override.
	LogLevel slog.Level `json:"logLevel"`
	// LogLevels maps component tags to overridden log levels. Tags are the
	// component names (etcd, api, controller, scheduler, webhook, kms,
	// jwtsigner) and the node names.
	LogLevels map[string]slog.Level `json:"logLevels,omitempty"`
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

	fs.Parse(os.Args[1:])

	if err := parseLogLevels(&c, logLevels); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log-level %q: %v\n", logLevels, err)
		os.Exit(1)
	}
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

// parseLevel parses a level name accepted by --log-level.
func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	}

	return 0, fmt.Errorf("unknown level %q (want error, warn, info or debug)", s)
}

// parseLogLevels splits a --log-level value into a default level and
// per-component overrides: bare entries set the default, tag=level entries
// set an override for that component.
func parseLogLevels(c *Config, s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}

	parsed := false
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if tag, lvlStr, ok := strings.Cut(part, "="); ok {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				return fmt.Errorf("empty component tag in %q", part)
			}
			lvl, err := parseLevel(strings.TrimSpace(lvlStr))
			if err != nil {
				return err
			}
			if c.LogLevels == nil {
				c.LogLevels = map[string]slog.Level{}
			}
			c.LogLevels[tag] = lvl
			parsed = true
			continue
		}

		lvl, err := parseLevel(part)
		if err != nil {
			return err
		}
		c.LogLevel = lvl
		parsed = true
	}

	if !parsed {
		return fmt.Errorf("no levels specified")
	}

	return nil
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
