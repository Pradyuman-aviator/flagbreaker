// Command checkout is the demo workload. It reads two flags from Flagsmith
// with the official Go SDK:
//
//   - new_pricing: while it is on, the "new pricing engine" fails and the
//     process exits, so the pods crash-loop. This bug is staged for the demo;
//     Flagsmith, Kubernetes and Flagbreaker are real.
//   - express_checkout: changes what the page says and nothing else. The
//     demo's (staged) Grafana alert blames it for an error rate the pods
//     can't see, which is the case for following other Feature Health
//     providers.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	flagsmith "github.com/Flagsmith/flagsmith-go-client/v5"
)

const (
	pricingFlag = "new_pricing"
	expressFlag = "express_checkout"
)

var express atomic.Bool

func main() {
	apiURL := strings.TrimRight(os.Getenv("FLAGSMITH_URL"), "/") + "/api/v1/"
	client := flagsmith.NewClient(os.Getenv("FLAGSMITH_ENVIRONMENT_KEY"),
		flagsmith.WithBaseURL(apiURL),
		flagsmith.WithRequestTimeout(3*time.Second),
	)

	checkFlags(client) // at startup
	go func() {
		for range time.Tick(2 * time.Second) {
			checkFlags(client)
		}
	}()

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		mode := "classic checkout"
		if express.Load() {
			mode = "express checkout"
		}
		fmt.Fprintf(w, "checkout: total $42.00 (classic pricing, %s)\n", mode)
	})
	log.Println("checkout listening on :8080 with classic pricing")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// checkFlags exits when new_pricing is on, like a service whose new code
// path crashes. If Flagsmith can't be reached, it keeps what it had.
func checkFlags(client *flagsmith.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	flags, err := client.GetEnvironmentFlags(ctx)
	if err != nil {
		log.Printf("could not read flags, keeping classic pricing: %v", err)
		return
	}
	if on, _ := flags.IsFeatureEnabled(expressFlag); on != express.Load() {
		express.Store(on)
		log.Printf("%s is now %v", expressFlag, on)
	}
	if on, _ := flags.IsFeatureEnabled(pricingFlag); on {
		log.Printf("%s is on: loading the new pricing engine", pricingFlag)
		log.Fatal("panic: pricing engine: price table v2 not found")
	}
}
