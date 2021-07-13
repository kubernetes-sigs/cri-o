package main

import (
	"os"

	"github.com/cri-o/cri-o/pkg/criu"
)

func main() {
	if !criu.CheckForCriu() {
		os.Exit(1)
	}

	os.Exit(0)
}
