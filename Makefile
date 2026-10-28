.PHONY: build test run

# Go 1.21 needs the SDK path and external linker on recent macOS. Newer Go and
# Linux use the regular build. Local ad hoc signing repairs the legacy ld output.
LOCAL_LINK_FLAGS :=
LOCAL_SIGN := true
ifeq ($(shell uname -s),Darwin)
ifneq ($(filter go1.21%,$(shell go env GOVERSION)),)
MACOS_SDK_PATH := $(shell xcrun --sdk macosx --show-sdk-path)
export CGO_CFLAGS := $(CGO_CFLAGS) -isysroot $(MACOS_SDK_PATH)
export CGO_LDFLAGS := $(CGO_LDFLAGS) -isysroot $(MACOS_SDK_PATH)
LOCAL_LINK_FLAGS := -ldflags=-linkmode=external
LOCAL_SIGN := codesign --force --sign -
endif
endif

build:
	go build -trimpath $(LOCAL_LINK_FLAGS) -o bin/conversion-api ./cmd/conversion-api
	go build -trimpath $(LOCAL_LINK_FLAGS) -o bin/send-test ./cmd/send-test
	$(LOCAL_SIGN) bin/conversion-api
	$(LOCAL_SIGN) bin/send-test

test:
	go test -race $(LOCAL_LINK_FLAGS) ./...
	go vet ./...

run: build
	./bin/conversion-api
