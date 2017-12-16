package main

import (
	"expvar"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"

	logcache "code.cloudfoundry.org/log-cache"
	"google.golang.org/grpc"
)

func main() {
	log.Print("Starting Log Cache...")
	defer log.Print("Closing Log Cache.")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("invalid configuration: %s", err)
	}

	cache := logcache.New(
		logcache.WithStoreSize(cfg.StoreSize),
		logcache.WithLogger(log.New(os.Stderr, "", log.LstdFlags)),
		logcache.WithMetrics(expvar.NewMap("LogCache")),
		logcache.WithAddr(cfg.Addr),
		logcache.WithClustered(cfg.NodeIndex, cfg.NodeAddrs, grpc.WithInsecure()),
	)

	cache.Start()

	// health endpoints (pprof and expvar)
	log.Printf("Health: %s", http.ListenAndServe(fmt.Sprintf("localhost:%d", cfg.HealthPort), nil))
}
