GO      ?= go
GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)
GUEST   := internal/vm/guest/aic-guest-linux-$(GOARCH)

.PHONY: all aic guest test clean

all: aic

# On macOS, aic embeds the Linux guest that runs in its build VM and must
# be signed with the virtualization entitlement.
aic:
ifeq ($(GOOS),darwin)
	$(MAKE) guest
	$(GO) build -o aic ./cmd/aic
	codesign --force --sign - --entitlements aic.entitlements aic
else
	$(GO) build -o aic ./cmd/aic
endif

guest:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags="-s -w" -o $(GUEST) ./cmd/aic-guest

test:
	$(GO) test ./...

clean:
	rm -f aic $(GUEST)
