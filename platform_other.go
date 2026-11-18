//go:build !windows

package main

func defaultDBPath() string { return "lifelog.db" }

func platformCommand(string) (bool, error) { return false, nil }

func runPlatform(cfg config) error { return runInteractive(cfg) }
