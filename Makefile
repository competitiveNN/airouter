# Build targets. The default builds all binaries; the standalone Command Code
# proxy and provider proxy are the same package compiled with the
# commandcode_proxy / airouter_proxy tags, which swap main.go for their own
# entry point and leave the gateway out.

GO           ?= go
BIN          ?= airouter
CC_BIN       ?= commandcode-proxy
AIROUTER_PROXY_BIN ?= airouter-proxy
GOFLAGS      ?=

.PHONY: all build commandcode-proxy airouter-proxy test vet clean

all: build airouter-proxy

build:
	$(GO) build $(GOFLAGS) -o $(BIN) .

commandcode-proxy:
	$(GO) build $(GOFLAGS) -tags commandcode_proxy -o $(CC_BIN) .

airouter-proxy:
	$(GO) build $(GOFLAGS) -tags airouter_proxy -o $(AIROUTER_PROXY_BIN) .

test:
	$(GO) test $(GOFLAGS) -count=1 ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

clean:
	rm -f $(BIN) $(CC_BIN) $(AIROUTER_PROXY_BIN)
