// SPDX-License-Identifier: Apache-2.0

// Command smi supplies a synthetic T4 to the rehearsal agent.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "--query-gpu=index,uuid,name,pci.bus_id":
		fmt.Println("0, GPU-00000000-0000-4000-8000-000000000124, Tesla T4, 00000000:00:1E.0")
	case "--query-gpu=count":
		fmt.Println("1")
	case "--query-gpu=mig.mode.current":
		fmt.Println("[N/A]")
	case "--query-gpu=driver_version":
		fmt.Println("580.178.04")
	default:
		fmt.Fprintln(os.Stderr, "unsupported synthetic nvidia-smi query")
		os.Exit(1)
	}
}
