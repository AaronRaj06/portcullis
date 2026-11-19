// Command portcullis-controller is a Phase 0 skeleton: it only reports its version.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/AaronRaj06/portcullis/internal"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("portcullis-controller", internal.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "portcullis-controller: not implemented yet (Phase 0 scaffold); see docs/ROADMAP.md")
	os.Exit(1)
}
