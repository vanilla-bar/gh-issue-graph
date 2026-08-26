VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet web-test ui-test run check clean

build:
	go build -ldflags "$(LDFLAGS)" -o gh-issue-graph ./cmd/gh-issue-graph

test:
	go test ./...

vet:
	go vet ./...

web-test:
	node --check internal/server/web/app.js
	node --check internal/server/web/refresh.js
	node --test internal/server/webtest/refresh_test.js

# Drives the real frontend in Chrome with real mouse input, through the
# DevTools Protocol. Skipped when Chrome is absent.
ui-test:
	python3 ./internal/server/webtest/ui-check.py

run:
	go run -ldflags "$(LDFLAGS)" ./cmd/gh-issue-graph

demo:
	GH_ISSUE_GRAPH_DEMO=1 go run -ldflags "$(LDFLAGS)" ./cmd/gh-issue-graph

check: test vet web-test

clean:
	rm -f gh-issue-graph
	rm -rf dist
