# Variables
BINARY_NAME=milvus-backup
PKG := github.com/zilliztech/milvus-backup
BUILD_VERSION=$(if $(VERSION),$(VERSION),$(shell git describe --tags --always))
BUILD_COMMIT=$(if $(COMMIT),$(COMMIT),$(shell git rev-parse --short HEAD))
DATE=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS += -X "$(PKG)/version.Version=$(BUILD_VERSION)"
LDFLAGS += -X "$(PKG)/version.Commit=$(BUILD_COMMIT)"
LDFLAGS += -X "$(PKG)/version.Date=$(DATE)"

# Default target
all: gen build

test:
	@echo "Running unit tests..."
	@go test --race ./...

# Build the binary
build:
	@echo "Building Backup binary..."
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Date: $(DATE)"
	@GO111MODULE=on CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY_NAME)

gen:
	./scripts/gen_swag.sh
	./scripts/gen_proto.sh

fmt:
	@echo Formatting code...
	@goimports -w --local $(PKG) ./
	@echo Format code done

.PHONY: all build gen