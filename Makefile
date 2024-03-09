CURRENT_DIR=$(shell pwd)
DIST_DIR=${CURRENT_DIR}/dist

.PHONY: build-debug
build-plugin-debug:
	CGO_ENABLED=0 go build -gcflags="all=-N -l" -o honeycomb-metric-plugin main.go

.PHONY: build
build-plugin:
	CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags="-s -w" -o ${DIST_DIR}/${BIN_NAME} main.go

.PHONY: release
release:
	make GOOS=linux GOARCH=amd64 BIN_NAME=honeycomb-metric-plugin-linux-amd64 build-plugin
	make GOOS=linux GOARCH=arm64 BIN_NAME=honeycomb-metric-plugin-linux-arm64 build-plugin
	make GOOS=darwin GOARCH=amd64 BIN_NAME=honeycomb-metric-plugin-darwin-amd64 build-plugin
	make GOOS=darwin GOARCH=arm64 BIN_NAME=honeycomb-metric-plugin-linux-arm64 build-plugin
