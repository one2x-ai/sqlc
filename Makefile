CGO_ENABLED = 1
COMMIT_HASH := $(shell git --no-pager describe --tags --always --dirty)
LDFLAGS = "-X github.com/sqlc-dev/sqlc/internal/info.Version=$(COMMIT_HASH)-wicked-fork"
# macOS 15.4+ needs strchrnul fix
BUILD_ENV = $(shell [ "$$(uname -s)" = "Darwin" ] && [ "$$(sw_vers -productVersion | cut -d. -f1)" -ge 15 ] && echo 'MACOSX_DEPLOYMENT_TARGET=15.4 CGO_CFLAGS="-DHAVE_STRCHRNUL"')

.PHONY: build build-endtoend test test-ci test-examples test-endtoend regen start psql mysqlsh proto

build:
	$(BUILD_ENV) CGO_ENABLED=$(CGO_ENABLED) go build -ldflags=$(LDFLAGS) -o bin/ ./cmd/...

install:
	$(BUILD_ENV) CGO_ENABLED=$(CGO_ENABLED) go install -ldflags=$(LDFLAGS) ./cmd/...

test:
	CGO_ENABLED=$(CGO_ENABLED) go test ./...

vet:
	CGO_ENABLED=$(CGO_ENABLED) go vet ./...

test-examples:
	CGO_ENABLED=$(CGO_ENABLED) go test --tags=examples ./...

build-endtoend:
	cd ./internal/endtoend/testdata && CGO_ENABLED=$(CGO_ENABLED) go build ./...

test-ci: test-examples build-endtoend vet

regen: sqlc-dev sqlc-gen-json
	CGO_ENABLED=$(CGO_ENABLED) go run ./scripts/regenerate/

sqlc-dev:
	CGO_ENABLED=$(CGO_ENABLED) go build -o ~/bin/sqlc-dev ./cmd/sqlc/

sqlc-pg-gen:
	CGO_ENABLED=$(CGO_ENABLED) go build -o ~/bin/sqlc-pg-gen ./internal/tools/sqlc-pg-gen

sqlc-gen-json:
	CGO_ENABLED=$(CGO_ENABLED) go build -o ~/bin/sqlc-gen-json ./cmd/sqlc-gen-json

start:
	docker-compose up -d

fmt:
	go fmt ./...

psql:
	PGPASSWORD=mysecretpassword psql --host=127.0.0.1 --port=5432 --username=postgres dinotest

mysqlsh:
	mysqlsh --sql --user root --password mysecretpassword --database dinotest 127.0.0.1:3306

proto:
	buf generate

remote-proto:
	protoc \
		--go_out=. --go_opt="Minternal/remote/gen.proto=github.com/sqlc-dev/sqlc/internal/remote" --go_opt=module=github.com/sqlc-dev/sqlc \
        --go-grpc_out=. --go-grpc_opt="Minternal/remote/gen.proto=github.com/sqlc-dev/sqlc/internal/remote" --go-grpc_opt=module=github.com/sqlc-dev/sqlc \
        internal/remote/gen.proto
