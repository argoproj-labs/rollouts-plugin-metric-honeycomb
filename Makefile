.PHONY: build-debug
build-plugin-debug:
	CGO_ENABLED=0 go build -gcflags="all=-N -l" -o honeycomb-metric-plugin main.go

.PHONY: build
build-plugin:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o honeycomb-metric-plugin-linux-amd64 main.go
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o honeycomb-metric-linux-arm64 main.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o honeycomb-metric-darwin-amd64 main.go
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o honeycomb-metric-darwin-arm64 main.go
