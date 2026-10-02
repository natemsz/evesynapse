package main

import (
	"context"
	"log"
	"time"
)

// runWorker is the skeleton background loop. It will grow into the ESI
// refresh scheduler (character sheets, market orders, ...) honoring each
// call's cached_until the way the 2013 APITimer model did. For now it
// just proves the goroutine lives alongside the HTTP server.
func runWorker(ctx context.Context) {
	log.Printf("worker: started")
	log.Printf("worker heartbeat: boot %s", time.Now().UTC().Format(time.RFC3339))

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: stopped")
			return
		case t := <-ticker.C:
			log.Printf("worker heartbeat: %s", t.UTC().Format(time.RFC3339))
		}
	}
}
