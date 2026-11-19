.PHONY: build test vet check deploy

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

check: build vet test

# Build for the VM and install it there. See deploy/RUNBOOK.md.
deploy:
	deploy/deploy.sh
