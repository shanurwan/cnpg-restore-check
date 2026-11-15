CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
HELM ?= helm
PROMTOOL ?= promtool
IMG ?= ghcr.io/shanurwan/cnpg-restore-check:dev
CHART := charts/cnpg-restore-check

.PHONY: all
all: generate manifests test

.PHONY: generate
generate: ## Generate deepcopy code.
	$(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: ## Generate the CRD and RBAC rules, and copy them into the chart.
	$(CONTROLLER_GEN) crd rbac:roleName=cnpg-restore-check paths=./... output:crd:dir=config/crd output:rbac:dir=config/rbac
	cp config/crd/*.yaml $(CHART)/crds/
	sed -n '/^rules:/,$$p' config/rbac/role.yaml | tail -n +2 > $(CHART)/files/rules.yaml

.PHONY: test
test: ## Unit tests.
	go test -race ./...

.PHONY: test-alerts
test-alerts: ## Check and unit test the Prometheus rules.
	$(PROMTOOL) check rules $(CHART)/files/alerts.yaml
	$(PROMTOOL) test rules test/alerts/alerts_test.yaml

.PHONY: lint-chart
lint-chart:
	$(HELM) lint $(CHART)
	$(HELM) template x $(CHART) --set metrics.serviceMonitor.enabled=true --set prometheusRule.enabled=true --set leaderElection.enabled=true > /dev/null

.PHONY: install-yaml
install-yaml: ## Render a plain install manifest (CRD, RBAC, Deployment) to dist/install.yaml.
	mkdir -p dist
	cat $(CHART)/crds/*.yaml > dist/install.yaml
	echo '---' >> dist/install.yaml
	$(HELM) template cnpg-restore-check $(CHART) --namespace cnpg-restore-check >> dist/install.yaml

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: e2e
e2e: ## End-to-end tests against the current kubectl context (see test/e2e/README.md).
	go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/...
