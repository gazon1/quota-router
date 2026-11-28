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

Write a starter configuration and print where it went.

With no argument the file goes to the path the daemon reads: the one named by
QUOTA_ROUTER_CONFIG_FILE when that variable is set, otherwise the user-level
path, which on this system is:
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
		fmt.Fprintf(stderr, initConfigUsage, defaultConfigPathHint())
		return 2
	}
	// -h is help, not a path: treating it as one would create a file named "-h"
	// in the working directory, which is never what a caller asking for help
	// meant.
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintf(stdout, initConfigUsage, defaultConfigPathHint())
		return 0
	}
	path := ""
	if len(args) == 1 {
		path = args[0]
	}
	if path == "" {
		// The same rule the daemon uses: an explicit QUOTA_ROUTER_CONFIG_FILE
		// wins, so a file this command creates is the file the daemon reads.
		// Otherwise fall back to the user-level path.
		if p := config.ConfigPath(); p != "" {
			path = p
		} else {
			p, err := config.UserConfigPath()
			if err != nil {
				fmt.Fprintf(stderr, "init-config: %v; pass an explicit path\n", err)
				return 1
			}
			path = p
		}
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

// defaultConfigPathHint names the path the command would write to, so the usage
// text states the same answer the command acts on. It falls back to a
// description when no home directory can be resolved.
func defaultConfigPathHint() string {
	if path := config.ConfigPath(); path != "" {
		return path
	}
	if path, err := config.UserConfigPath(); err == nil {
		return path
	}
	return "(unresolved: no user config directory; pass a path)"
}

// helpText lists the subcommands. The daemon takes no flags — every setting comes
// from the file and the environment — so this stays short.
func helpText() string {
	return "usage: quota-controller [init-config]\n\n" +
		"Run with no subcommand to serve; configuration comes from " + defaultConfigPathHint() +
		" and the environment.\n\n" +
		"  init-config [path]   write a starter configuration and print where it went\n"
}
