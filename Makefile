# Build targets. The default builds both binaries; the standalone Command Code
# proxy is the same package compiled with the commandcode_proxy tag, which
# swaps main.go for commandcode_proxy_main.go and leaves the gateway out.

GO       ?= go
BIN      ?= airouter
CC_BIN   ?= commandcode-proxy
GOFLAGS  ?=

.PHONY: all build commandcode-proxy test vet clean

all: build commandcode-proxy

build:
	$(GO) build $(GOFLAGS) -o $(BIN) .

commandcode-proxy:
	$(GO) build $(GOFLAGS) -tags commandcode_proxy -o $(CC_BIN) .

test:
	$(GO) test $(GOFLAGS) -count=1 ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

clean:
	rm -f $(BIN) $(CC_BIN)
