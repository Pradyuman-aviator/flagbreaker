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
//   - checkout_config: remote config, a JSON value such as
//     {"tax_rate": 0.08}. The app reads it whether or not the flag is on,
//     like the SDK's GetFeatureValue does, and exits on a value it can't
//     parse, like a service that trusts its config. Also staged.
//
// It serves a small checkout page on :8080 that refreshes itself, so the demo
// shows the shop going down and coming back.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
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
	configFlag  = "checkout_config"

	subtotal = 42.00
)

var (
	express atomic.Bool
	// taxRate holds the float64 bits of checkout_config's tax_rate.
	taxRate atomic.Uint64
)

// checkoutConfig is checkout_config's value.
type checkoutConfig struct {
	TaxRate float64 `json:"tax_rate"`
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="2">
<title>Acme checkout</title>
<style>
  body { font-family: system-ui, sans-serif; background: #f6f6f8; color: #1d1d1f; margin: 0; }
  main { max-width: 26rem; margin: 3rem auto; padding: 0 1rem; }
  .card { background: #fff; border: 1px solid #e2e2e6; border-radius: 10px; padding: 1.5rem; }
  h1 { font-size: 1.1rem; margin: 0 0 1rem; }
  .total { font-size: 2.2rem; font-weight: 650; margin: .25rem 0 1rem; }
  .row { display: flex; justify-content: space-between; padding: .4rem 0; border-top: 1px solid #eee; }
  .on { color: #176f2c; font-weight: 600; }
  .muted { color: #6e6e73; font-size: .8rem; margin-top: 1rem; }
  .up { color: #176f2c; font-weight: 600; }
</style>
</head>
<body>
<main>
  <div class="card">
    <h1>Acme checkout</h1>
    <div class="total">${{.Total}}</div>
    <div class="row"><span>Subtotal</span><span>${{.Subtotal}}</span></div>
    <div class="row"><span>Tax ({{.TaxPercent}}, from checkout_config)</span><span>${{.Tax}}</span></div>
    <div class="row"><span>Pricing</span><span>classic</span></div>
    <div class="row"><span>Checkout</span><span{{if .Express}} class="on"{{end}}>{{if .Express}}express (one click){{else}}standard{{end}}</span></div>
    <div class="row"><span>Status</span><span class="up">&#9679; up</span></div>
  </div>
  <p class="muted">Served by {{.Pod}} at {{.Time}} UTC. Flags come from Flagsmith; this page refreshes every 2 seconds.</p>
</main>
</body>
</html>
`))

func main() {
	apiURL := strings.TrimRight(os.Getenv("FLAGSMITH_URL"), "/") + "/api/v1/"
	client := flagsmith.NewClient(os.Getenv("FLAGSMITH_ENVIRONMENT_KEY"),
		flagsmith.WithBaseURL(apiURL),
		flagsmith.WithRequestTimeout(3*time.Second),
	)

	taxRate.Store(math.Float64bits(0.08))
	checkFlags(client) // at startup
	go func() {
		for range time.Tick(time.Second) {
			checkFlags(client)
		}
	}()

	pod, _ := os.Hostname()
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		rate := math.Float64frombits(taxRate.Load())
		tax := subtotal * rate
		_ = page.Execute(w, map[string]any{
			"Express":    express.Load(),
			"Subtotal":   fmt.Sprintf("%.2f", subtotal),
			"TaxPercent": fmt.Sprintf("%g%%", rate*100),
			"Tax":        fmt.Sprintf("%.2f", tax),
			"Total":      fmt.Sprintf("%.2f", subtotal+tax),
			"Pod":        pod,
			"Time":       time.Now().UTC().Format(time.TimeOnly),
		})
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
	if raw, err := flags.GetFeatureValue(configFlag); err == nil && raw != nil {
		var c checkoutConfig
		if err := json.Unmarshal([]byte(fmt.Sprint(raw)), &c); err != nil {
			log.Fatalf("panic: %s: %v", configFlag, err)
		}
		if bits := math.Float64bits(c.TaxRate); bits != taxRate.Load() {
			taxRate.Store(bits)
			log.Printf("%s: tax rate is now %g", configFlag, c.TaxRate)
		}
	}
}
