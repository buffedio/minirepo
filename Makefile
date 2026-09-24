# minirepo Makefile —— 版本跟随 git tag

BINARY  := minirepo
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

PLATFORMS := \
	linux/amd64 linux/arm64 \
	windows/amd64 \
	darwin/arm64 darwin/amd64

.PHONY: all build release clean

all: build

# CGO_ENABLED=0:纯静态二进制。默认构建会链 glibc,拿到老 CentOS/Alpine 上
# 就跑不起来("GLIBC_2.xx not found"),而"拷过去就能用"是本工具的主要卖点
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .
	@echo "built $(BINARY) $(VERSION)"

# 全平台交叉编译，产物进 dist/
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" -o $$out . ; \
		echo "built $$out ($(VERSION))"; \
	done

clean:
	rm -rf dist $(BINARY) $(BINARY).exe
