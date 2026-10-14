.PHONY: default lint vet test test-race bench yaegi-test yaegi-bench bench-traefik bench-all tidy vendor clean

export GO111MODULE=on

# Module path of the plugin; Yaegi resolves it through a GOPATH tree.
MODULE      := github.com/gabay/maxmind-geoblock
YAEGI       ?= yaegi
YAEGI_GOPATH := $(CURDIR)/.yaegi-gopath
BENCHTIME   ?= 1s

default: test yaegi-test

## Static checks

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | test -z "$$(cat)"

## Native Go toolchain

test:
	go test -v -cover ./...

test-race:
	go test -race ./...

bench:
	go test -run xxx -bench . -benchmem -benchtime $(BENCHTIME) ./...

## Yaegi (the interpreter Traefik embeds)

# Traefik loads plugins from a GOPATH-style tree, so expose the checkout there.
$(YAEGI_GOPATH)/src/$(MODULE):
	mkdir -p $(dir $@)
	ln -sfn $(CURDIR) $@

yaegi-test: $(YAEGI_GOPATH)/src/$(MODULE)
	cd $(YAEGI_GOPATH)/src/$(MODULE) && GOPATH=$(YAEGI_GOPATH) $(YAEGI) test -v .
	cd $(YAEGI_GOPATH)/src/$(MODULE)/mmdb && GOPATH=$(YAEGI_GOPATH) $(YAEGI) test -v .

yaegi-bench: $(YAEGI_GOPATH)/src/$(MODULE)
	cd $(YAEGI_GOPATH)/src/$(MODULE) && GOPATH=$(YAEGI_GOPATH) $(YAEGI) test -run xxx -bench . -benchmem -benchtime $(BENCHTIME) .
	cd $(YAEGI_GOPATH)/src/$(MODULE)/mmdb && GOPATH=$(YAEGI_GOPATH) $(YAEGI) test -run xxx -bench . -benchmem -benchtime $(BENCHTIME) .

# Loads the plugin into an embedded Yaegi interpreter exactly like Traefik
# does and benchmarks it side by side with the compiled plugin.
bench-traefik:
	cd tools/yaegibench && go test -run TestYaegiMatchesNative -v . && go test -run xxx -bench . -benchmem -benchtime $(BENCHTIME) .

bench-all: bench yaegi-bench bench-traefik

## Housekeeping

tidy:
	go mod tidy
	cd tools/yaegibench && go mod tidy

# The plugin has no third-party dependencies, so there is nothing to vendor;
# kept for parity with the Traefik plugin template.
vendor:
	go mod vendor

clean:
	rm -rf ./vendor $(YAEGI_GOPATH)
