package criu

import (
	"github.com/checkpoint-restore/go-criu"
)

// This file is a copy from Podman pkg/criu/criu.go.
// The only difference is the change from 11 to 16 because
// CRIU 3.16 is required for CRI-O.

// MinCriuVersion for CRI-O is at least CRIU 3.16
const MinCriuVersion = 31600

// CheckForCriu uses CRIU's go bindings to check if the CRIU
// binary exists and if it at least the version CRI-O needs.
func CheckForCriu() bool {
	c := criu.MakeCriu()
	result, err := c.IsCriuAtLeast(MinCriuVersion)
	if err != nil {
		return false
	}
	return result
}
