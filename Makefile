GIT_COMMIT := $(shell git rev-parse HEAD)
GIT_DATE := $(shell git show -s --format='%ct')

LD_FLAGS_ARGS +=-X github.com/taikoxyz/taiko-client/version.GitCommit=$(GIT_COMMIT)
LD_FLAGS_ARGS +=-X github.com/taikoxyz/taiko-client/version.GitDate=$(GIT_DATE)

LD_FLAGS := -ldflags "$(LD_FLAGS_ARGS)"

build:
	GO111MODULE=on go build -v $(LD_FLAGS) -o bin/taiko-client cmd/main.go

clean:
	@rm -rf bin/*

lint:
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.46.2 \
	&& golangci-lint run

test:
	@TAIKO_MONO_DIR=${TAIKO_MONO_DIR} \
	COMPILE_PROTOCOL=${COMPILE_PROTOCOL} \
	PACKAGE=${PACKAGE} \
	RUN_TESTS=true \
		./integration_test/entrypoint.sh

dev_net:
	@TAIKO_MONO_DIR=${TAIKO_MONO_DIR} \
	COMPILE_PROTOCOL=${COMPILE_PROTOCOL} \
		./integration_test/entrypoint.sh

gen_bindings:
	@TAIKO_MONO_DIR=${TAIKO_MONO_DIR} \
	TAIKO_GETH_DIR=${TAIKO_GETH_DIR} \
		./scripts/gen_bindings.sh

run_driver:
	@L1_ENDPOINT_WS=${L1_ENDPOINT_WS} \
	TAIKO_L1_ADDRESS=${TAIKO_L1_ADDRESS} \
	TAIKO_L2_ADDRESS=${TAIKO_L2_ADDRESS} \
		./scripts/start-driver.sh

run_proposer:
	@ENABLE_PROPOSER=${ENABLE_PROPOSER} \
	L1_ENDPOINT_WS=${L1_ENDPOINT_WS} \
	TAIKO_L1_ADDRESS=${TAIKO_L1_ADDRESS} \
	TAIKO_L2_ADDRESS=${TAIKO_L2_ADDRESS} \
	L1_PROPOSER_PRIVATE_KEY=${L1_PROPOSER_PRIVATE_KEY} \
	L2_SUGGESTED_FEE_RECIPIENT=${L2_SUGGESTED_FEE_RECIPIENT} \
		./scripts/start-proposer.sh

.PHONY: build \
				clean \
				lint \
				test \
				dev_net \
				gen_bindings
