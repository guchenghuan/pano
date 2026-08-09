package main

import (
	"log"
	"os"
)

var debugLog *log.Logger

func init() {
	if path := os.Getenv("PANO_DEBUG"); path != "" {
		if f, err := os.Create(path); err == nil {
			debugLog = log.New(f, "", log.Lmicroseconds)
		}
	}
}

func debugf(format string, args ...any) {
	if debugLog != nil {
		debugLog.Printf(format, args...)
	}
}
