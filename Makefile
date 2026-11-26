.DEFAULT_GOAL := help

.PHONY: help tools test generate demo demo-break demo-alert demo-restore demo-test demo-down

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-13s %s\n", $$1, $$2}'

tools: ## Download pinned kind, kubectl and helm into ./bin
	bash hack/tools.sh

test: ## Run the unit and controller tests
	go test ./...

generate: ## Regenerate deepcopy code and the CRD from api/v1alpha1
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0 object paths=./api/v1alpha1 \
		crd:crdVersions=v1 output:crd:artifacts:config=charts/flagbreaker/crds

demo: ## Create a kind cluster with self-hosted Flagsmith, Flagbreaker and a demo app
	bash hack/demo/up.sh

demo-break: ## Switch new_pricing on, as a person would, and watch Flagbreaker react
	bash hack/demo/break.sh

demo-alert: ## Fire a Grafana alert for express_checkout (resolve with: bash hack/demo/alert.sh resolve)
	bash hack/demo/alert.sh fire

demo-restore: ## Restore new_pricing as it was before Flagbreaker switched it off
	bash hack/demo/restore.sh

demo-test: ## Run every end-to-end check: act, restore, watch mode and a Grafana alert
	bash hack/demo/e2e-all.sh

demo-down: ## Delete the demo cluster and its local state
	bash hack/demo/down.sh
