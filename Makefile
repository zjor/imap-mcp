.PHONY: test it lint

test:
	go test ./...

it:
	test -f test/integration/certs/server.pem || sh test/integration/gencerts.sh
	docker compose -f test/integration/compose.yaml up -d --wait && \
		go test -tags integration ./test/integration/... ; \
		rc=$$?; docker compose -f test/integration/compose.yaml down; exit $$rc

lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...
