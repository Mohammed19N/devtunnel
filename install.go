// Package passway holds the repository-level assets shared by the project,
// such as the one-line installer script served from the website.
package passway

import _ "embed"

// InstallScript is the contents of install.sh.
//
//go:embed install.sh
var InstallScript []byte
