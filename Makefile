# Build targets. The default builds all binaries; the standalone Command Code
# proxy and provider proxy are the same package compiled with the
# commandcode_proxy / provider_proxy tags, which swap main.go for their own
# entry point and leave the gateway out.

GO           ?= go
BIN          ?= airouter
CC_BIN       ?= commandcode-proxy
PROVIDER_BIN ?= provider-proxy
GOFLAGS      ?=

.PHONY: all build commandcode-proxy provider-proxy test vet clean

all: build commandcode-proxy provider-proxy

build:
	$(GO) build $(GOFLAGS) -o $(BIN) .

commandcode-proxy:
	$(GO) build $(GOFLAGS) -tags commandcode_proxy -o $(CC_BIN) .

provider-proxy:
	$(GO) build $(GOFLAGS) -tags provider_proxy -o $(PROVIDER_BIN) .

test:
	$(GO) test $(GOFLAGS) -count=1 ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

clean:
	rm -f $(BIN) $(CC_BIN) $(PROVIDER_BIN)
