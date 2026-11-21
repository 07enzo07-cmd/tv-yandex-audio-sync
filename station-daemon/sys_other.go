//go:build !linux

package main

import "os/exec"

func setPdeathsig(cmd *exec.Cmd) {}

func ensureLowLatAsoundMount() {}

func prioritizePlayback(pid int) error { return nil }
