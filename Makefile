.PHONY: test it lint chart-check

test:
	go test ./...

it:
	test -f test/integration/certs/server.pem || sh test/integration/gencerts.sh
	docker compose -f test/integration/compose.yaml up -d --wait && \
		go test -tags integration ./test/integration/... ; \
		rc=$$?; docker compose -f test/integration/compose.yaml down; exit $$rc

lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

# Helm chart: lint and render checks, no cluster needed.
chart-check:
	bash deploy/scripts/check-chart.sh
	bash deploy/scripts/test-ci-validate.sh

# Pretty logs with pplog (see pplog.env) when it is installed, raw JSON otherwise.
.PHONY: run
run:
	@if command -v pplog >/dev/null 2>&1; then IMAP_MCP_LOG_FORMAT=json pplog go run ./cmd/imap-mcp; \
	else echo "pplog not found, logs are raw JSON" >&2; go run ./cmd/imap-mcp; fi
