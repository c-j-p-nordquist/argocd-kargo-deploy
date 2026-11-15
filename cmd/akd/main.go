// Command akd installs and verifies a hardened Argo CD and Kargo platform.
package main

import (
	"os"

	"github.com/c-j-p-nordquist/argocd-kargo-deploy/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
