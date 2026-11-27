// Package templates carries files the binary writes out, embedded so an
// installed controller needs no data files beside it.
package templates

import _ "embed"

// ConfigExample is the starter configuration that `quota-controller init-config`
// writes. It is a copy of the repository's config.example.yaml, kept in sync by
// TestInitConfigTemplateMatchesExample.
//
//go:embed config.example.yaml
var ConfigExample []byte
