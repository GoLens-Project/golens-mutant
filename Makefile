.PHONY: build test cover cover-html cover-func vet fmt fmt-check ci smoke clean install

# Binaries
GO ?= go
COVERAGE_FILE ?= coverage.txt

build:
	$(GO) build ./...

# Run the full test suite with the race detector.
test:
	$(GO) test -race ./...

# Run tests with coverage and print a per-function report.
cover:
	$(GO) test -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVERAGE_FILE)

# Same as cover, but opens an HTML report in the browser.
cover-html: $(COVERAGE_FILE)
	$(GO) tool cover -html=$(COVERAGE_FILE)

$(COVERAGE_FILE):
	$(GO) test -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w cmd internal

# Fail (don't fix) on formatting drift — what CI enforces.
fmt-check:
	@unformatted=$$(gofmt -l cmd internal); \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt needed on:" $$unformatted; \
	  exit 1; \
	fi

# Everything CI runs, in the same order.
ci: fmt-check vet test

# Build the CLI and run it against a toy workspace.
smoke:
	$(GO) build -o /tmp/mutant-smoke ./cmd/mutant
	@tmp=$$(mktemp -d); \
	mkdir -p $$tmp/libs/demo/lib; \
	echo "name: demo" > $$tmp/libs/demo/pubspec.yaml; \
	echo "int a() => 1;" > $$tmp/libs/demo/lib/a.dart; \
	printf 'mutation:\n  target_roots: [libs]\n  file_patterns: ["lib/**/*.dart"]\n  tests:\n    - pattern: "lib/**"\n      tests: [t]\n  commands: ["echo mutating {package}/{file} && exit 1"]\nresources:\n  spawn_cooldown: 0s\nworkspace:\n  bootstrap: ""\n' > $$tmp/smoke.yaml; \
	cd $$tmp && /tmp/mutant-smoke --config smoke.yaml && \
	grep -q '"killed"' reports/resume-state.json && \
	echo "smoke: OK"; \
	rm -rf $$tmp

install:
	$(GO) install ./cmd/mutant

clean:
	rm -f $(COVERAGE_FILE) /tmp/mutant-smoke
