package gocache

import "log"

func logf(format string, args ...interface{}) {
	log.Printf("brine-gocache: "+format, args...)
}
