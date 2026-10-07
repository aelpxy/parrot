// Package config loads parrot configuration files.
package config

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/aelpxy/parrot/internal/tunnel"
)

// ProjectFile is the per-project config file name, looked up in the working directory.
const ProjectFile = "parrot.toml"

// Config is the decoded contents of a project or user config file.
type Config struct {
	Token   string
	Server  string
	Tunnels []tunnel.Spec
}

type fileSchema struct {
	Tunnels map[string]toml.Primitive `toml:"tunnels"`
	Token   string                    `toml:"token"`
	Server  string                    `toml:"server"`
}

type tableSchema struct {
	Target     target       `toml:"target"`
	Subdomain  string       `toml:"subdomain"`
	Auth       string       `toml:"auth"`
	Host       string       `toml:"host"`
	Proto      tunnel.Proto `toml:"proto"`
	RemotePort int          `toml:"remote_port"`
	Autostart  bool         `toml:"autostart"`
}

// target accepts both `3000` and "host:port".
type target string

// UnmarshalTOML implements toml.Unmarshaler.
func (t *target) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		*t = target(strconv.FormatInt(v, 10))
	case string:
		*t = target(v)
	default:
		return fmt.Errorf("target must be a port number or string, got %T", v)
	}

	return nil
}

// UserPath returns the path of the per-user config file.
func UserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}

	return filepath.Join(dir, "parrot", "config.toml"), nil
}

// LoadFile reads and decodes the config file at path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}

	cfg, err := Load(f)

	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return cfg, nil
}

// Load decodes a config file, rejecting unknown keys and invalid tunnels.
func Load(r io.Reader) (*Config, error) {
	var raw fileSchema

	md, err := toml.NewDecoder(r).Decode(&raw)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	tunnels, err := decodeTunnels(&md, raw.Tunnels)
	if err != nil {
		return nil, err
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("unknown key %q", undecoded[0].String())
	}

	return &Config{Token: raw.Token, Server: raw.Server, Tunnels: tunnels}, nil
}

func decodeTunnels(md *toml.MetaData, raw map[string]toml.Primitive) ([]tunnel.Spec, error) {
	specs := make([]tunnel.Spec, 0, len(raw))

	var errs []error

	for _, name := range slices.Sorted(maps.Keys(raw)) {
		spec, err := decodeTunnel(md, name, raw[name])
		if err != nil {
			errs = append(errs, err)

			continue
		}

		specs = append(specs, spec)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return specs, nil
}

func decodeTunnel(md *toml.MetaData, name string, p toml.Primitive) (tunnel.Spec, error) {
	var t tableSchema

	switch md.Type("tunnels", name) {
	case "Integer", "String":
		if err := md.PrimitiveDecode(p, &t.Target); err != nil {
			return tunnel.Spec{}, fmt.Errorf("tunnel %q: %w", name, err)
		}

		t.Proto, t.Target = splitShorthand(t.Target)
	case "Hash":
		if err := md.PrimitiveDecode(p, &t); err != nil {
			return tunnel.Spec{}, fmt.Errorf("tunnel %q: %w", name, err)
		}
	default:
		return tunnel.Spec{}, fmt.Errorf(`tunnel %q: expected a port, a "proto:target" string, or a table`, name)
	}

	if t.Proto == tunnel.ProtoInvalid {
		t.Proto = tunnel.HTTP
	}

	spec := tunnel.Spec{
		Name:       name,
		Proto:      t.Proto,
		Target:     string(t.Target),
		Subdomain:  t.Subdomain,
		Auth:       t.Auth,
		HostHeader: t.Host,
		RemotePort: t.RemotePort,
		Autostart:  t.Autostart,
	}

	if err := spec.Normalize(); err != nil {
		return tunnel.Spec{}, fmt.Errorf("tunnel %q: %w", name, err)
	}

	return spec, nil
}

// splitShorthand splits "tcp:5432" into its protocol and target, defaulting to http.
func splitShorthand(t target) (tunnel.Proto, target) {
	prefix, rest, ok := strings.Cut(string(t), ":")
	if !ok {
		return tunnel.HTTP, t
	}

	proto, err := tunnel.ParseProto(prefix)
	if err != nil {
		return tunnel.HTTP, t
	}

	return proto, target(rest)
}
