package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"

	envstruct "code.cloudfoundry.org/go-envstruct"
	. "code.cloudfoundry.org/log-cache/internal/scheduler"
	"google.golang.org/grpc"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	log.Print("Starting Log Cache Scheduler...")
	defer log.Print("Closing Log Cache Scheduler.")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("invalid configuration: %s", err)
	}

	envstruct.WriteReport(cfg)

	opts := []SchedulerOption{
		WithSchedulerLogger(log.New(os.Stderr, "", log.LstdFlags)),
		WithSchedulerInterval(cfg.Interval),
		WithSchedulerCount(cfg.Count),
		WithSchedulerReplicationFactor(cfg.ReplicationFactor),
		WithSchedulerDialOpts(
			grpc.WithTransportCredentials(cfg.TLS.Credentials("log-cache")),
		),
	}

	if cfg.LeaderElectionEndpoint != "" {
		opts = append(opts, WithSchedulerLeadership(func() bool {
			resp, err := http.Get(cfg.LeaderElectionEndpoint)
			if err != nil {
				log.Printf("failed to read from leaderhip endpoint: %s", err)
				return false
			}

			return resp.StatusCode == http.StatusOK
		}))
	}

	sched := NewScheduler(
		cfg.NodeAddrs,
		opts...,
	)

	sched.Start()

	// health endpoints (pprof)
	log.Printf("Health: %s", http.ListenAndServe(fmt.Sprintf("localhost:%d", cfg.HealthPort), nil))
}
