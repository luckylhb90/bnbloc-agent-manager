// Command bnbloc-agent-manager is the agent manager: an installable plugin
// component (docs/prd/2026-10-24-agent-manager.md in bnbloc) that shows the
// coding agents on a member's computer, installs the ones its signed catalog
// can, keeps them current and gives them to the ACP core. This build carries
// the public catalog; see package app for the command line and the environment.
package main

import (
	"log"
	"os"

	"github.com/luckylhb90/bnbloc-agent-manager/app"
	"github.com/luckylhb90/bnbloc-agent-manager/manager"
)

// version is set by the release build (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	cat, err := manager.DefaultCatalog()
	if err == nil {
		err = app.Main(os.Args[1:], cat, version)
	}
	if err != nil {
		log.Fatalf("bnbloc-agent-manager: %v", err)
	}
}
