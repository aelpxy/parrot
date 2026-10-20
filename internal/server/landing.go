package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
)

//go:embed landing.html
var landingHTML string

// renderLanding builds the page served on the bare domain, explaining what the server is.
func renderLanding(domain string, anonymous bool, sshCommand string) ([]byte, error) {
	tmpl, err := template.New("landing").Parse(landingHTML)
	if err != nil {
		return nil, fmt.Errorf("parse landing page: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Domain     string
		SSHCommand string
		Anonymous  bool
	}{domain, sshCommand, anonymous}); err != nil {
		return nil, fmt.Errorf("render landing page: %w", err)
	}

	return buf.Bytes(), nil
}
