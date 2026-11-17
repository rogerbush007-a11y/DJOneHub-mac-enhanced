//go:build !windows

package main

import "context"

func initPlatformRuntime() func() { return func() {} }

func withPlatformLifecycle(parent context.Context, _ string) (context.Context, func()) {
	return parent, func() {}
}

func openPlatformUI(string) {}

func platformOpenExistingUI(string) bool { return false }
