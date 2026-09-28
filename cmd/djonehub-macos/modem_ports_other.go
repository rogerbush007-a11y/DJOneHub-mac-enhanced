//go:build !linux

package main

import (
	"errors"

	"github.com/iniwex5/vohive/internal/modem"
)

// platformPreferredATPort has no fixed answer outside Linux; discovery probes
// the serial ports the system lists instead.
func platformPreferredATPort() (string, error) {
	return "", errors.New("no platform-preferred AT port")
}

func startPlatformWatchdog(*modem.Manager, string) {}
