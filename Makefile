.PHONY: test build fmt helm-template

fmt:
	go fmt ./...

test:
	go test ./...

build:
	go build ./cmd/api ./cmd/worker ./cmd/mcp

helm-template:
	helm template gitlab-doc-platform ./deploy/helm/gitlab-doc-platform
