package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gazon1/quota-router/cmd/quota-controller/templates"
	"github.com/gazon1/quota-router/internal/config"
)

// initConfigUsage is printed for `init-config -h`, so the one thing an operator
// can get wrong — where the file goes — is answerable without the README.
const initConfigUsage = `usage: quota-controller init-config [path]

Write a starter configuration to the user-level path and print it.

With no argument the file goes to the path Load reads by default, which on this
system is:
    %s

It is created only when absent; an existing file is never overwritten, so
settings and secrets already there survive an upgrade. An explicit path must be
given when no home directory can be resolved.

The file contains placeholders only. Credentials stay in the environment
(QUOTA_ROUTER_AUTH__TOKEN, QUOTA_ROUTER_PROVIDERS__<NAME>__API_KEY) or a mounted
secret file.
`

// initConfig writes the starter configuration and reports where it went. It is
// a subcommand of the same binary that reads the file, so the path it writes is
// by construction the path Load looks at — the two cannot drift.
//
// It refuses to overwrite: the file outlives a reinstall by design, so clobbering
// it would destroy exactly what this feature exists to preserve.
func initConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprint(stderr, fmt.Sprintf(initConfigUsage, defaultConfigPathHint()))
		return 2
	}
	// -h is help, not a path: treating it as one would create a file named "-h"
	// in the working directory, which is never what a caller asking for help
	// meant.
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, fmt.Sprintf(initConfigUsage, defaultConfigPathHint()))
		return 0
	}
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	if path == "" {
		p, err := config.UserConfigPath()
		if err != nil {
			fmt.Fprintf(stderr, "init-config: %v; pass an explicit path\n", err)
			return 1
		}
		path = p
	}
	// 0700 for the directory and 0600 for the file: the configuration may be
	// edited in place with real values later, and a world-readable copy of it is
	// not something the operator should have to fix after the fact.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(stderr, "init-config: %v; pass an explicit writable path\n", err)
		return 1
	}
	if err := writeNewFile(path, templates.ConfigExample); err != nil {
		fmt.Fprintf(stderr, "init-config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return 0
}

// writeNewFile creates path exclusively, so a file that appeared between the
// existence check and the write is not silently replaced. A write or close that
// fails removes the partial file: leaving it behind would make every later run
// refuse with "already exists" because of a file that was never valid.
func writeNewFile(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; not overwriting it", path)
		}
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// defaultConfigPathHint names the default path in the usage text, falling back to
// a description when no home directory can be resolved.
func defaultConfigPathHint() string {
	path, err := config.UserConfigPath()
	if err != nil {
		return "(unresolved: no user config directory; pass a path)"
	}
	return path
}

// helpText lists the subcommands. The daemon takes no flags — every setting comes
// from the file and the environment — so this stays short.
func helpText() string {
	return "usage: quota-controller [init-config]\n\n" +
		"Run with no subcommand to serve; configuration comes from " + defaultConfigPathHint() +
		" and the environment.\n\n" +
		"  init-config [path]   write a starter configuration and print where it went\n"
}
