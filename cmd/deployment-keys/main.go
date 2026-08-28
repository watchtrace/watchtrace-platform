// Command deployment-keys generates or verifies the host-only deployment keys.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/watchtrace/watchtrace-platform/internal/deploymentkeys"
)

func main() {
	mode := flag.String("mode", "verify", "generate or verify")
	directory := flag.String("directory", "", "existing key directory")
	flag.Parse()

	if *directory == "" {
		fmt.Fprintln(os.Stderr, "deployment key operation failed: directory is required")
		os.Exit(1)
	}
	var err error
	action := "verified"
	switch *mode {
	case "generate":
		err = deploymentkeys.Generate(*directory)
		action = "generated"
	case "verify":
		err = deploymentkeys.Verify(*directory)
	default:
		err = fmt.Errorf("unsupported mode")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "deployment key operation failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("deployment keys %s successfully; no key material was printed\n", action)
}
