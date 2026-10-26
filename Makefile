GO      ?= go
GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)
GUEST   := internal/vm/guest/aic-guest-linux-$(GOARCH)
BINDIR  ?= $(or $(shell $(GO) env GOBIN),$(shell $(GO) env GOPATH)/bin)

.PHONY: all aic guest sdkcheck install test clean

# The vz package needs headers up to the macOS 15 SDK.
MIN_SDK := 15

all: aic

# On macOS, aic embeds the Linux guest that runs in its build VM and must
# be signed with the virtualization entitlement.
aic:
ifeq ($(GOOS),darwin)
	$(MAKE) sdkcheck
	$(MAKE) guest
	$(GO) build -o aic ./cmd/aic
	codesign --force --sign - --entitlements aic.entitlements aic
else
	$(GO) build -o aic ./cmd/aic
endif

# A stale Command Line Tools install (e.g. one that survived several macOS
# upgrades) can leave the active SDK years behind the OS, which breaks the
# cgo build of github.com/Code-Hex/vz with errors like
# "unknown type name 'VZNetworkDevice'". Catch that before compiling.
sdkcheck:
	@sdk=$$(xcrun --show-sdk-version 2>/dev/null); \
	major=$${sdk%%.*}; \
	if [ -z "$$sdk" ] || [ "$$major" -lt $(MIN_SDK) ]; then \
		echo "error: macOS SDK $${sdk:-not found} is too old; aic needs the macOS $(MIN_SDK)+ SDK" >&2; \
		echo "       (OS version: $$(sw_vers -productVersion), clang: $$(clang --version 2>/dev/null | head -1))" >&2; \
		echo "" >&2; \
		echo "Your Xcode Command Line Tools are probably stale. To update them, run:" >&2; \
		echo "" >&2; \
		echo "    touch /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress" >&2; \
		echo "    softwareupdate --list 2>&1 | grep 'Command Line Tools'" >&2; \
		echo "    softwareupdate --install 'Command Line Tools for Xcode <version>-<version>'" >&2; \
		echo "    rm /tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress" >&2; \
		echo "" >&2; \
		echo "or run 'xcode-select --install'. If that reports the tools are already" >&2; \
		echo "installed, remove /Library/Developer/CommandLineTools first and retry." >&2; \
		echo "Check with: xcrun --show-sdk-version" >&2; \
		exit 1; \
	fi

guest:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags="-s -w" -o $(GUEST) ./cmd/aic-guest

# Copying keeps the code signature, which is embedded in the binary.
install: aic
	install -d $(BINDIR)
	install -m 0755 aic $(BINDIR)/aic

test:
	$(GO) test ./...

clean:
	rm -f aic $(GUEST)
