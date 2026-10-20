package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Server is a server config file. Every key matches a 'parrot server' flag with dashes turned into
// underscores; flags given on the command line win over the file.
type Server struct {
	Domain         string `toml:"domain"`
	Listen         string `toml:"listen"`
	HTTP           string `toml:"http"`
	HTTPS          string `toml:"https"`
	TCPPorts       string `toml:"tcp_ports"`
	TLSCert        string `toml:"tls_cert"`
	TLSKey         string `toml:"tls_key"`
	DataDir        string `toml:"data_dir"`
	SSH            string `toml:"ssh"`
	AllowAnonymous bool   `toml:"allow_anonymous"`
	NoWarning      bool   `toml:"no_warning"`
	SSHOnHTTPS     bool   `toml:"ssh_on_https"`
}

// LoadServerFile reads a server config file, rejecting keys it does not know so typos don't go unnoticed.
func LoadServerFile(path string) (*Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read server config: %w", err)
	}

	var s Server

	md, err := toml.Decode(string(data), &s)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", path, undecoded[0].String())
	}

	return &s, nil
}
