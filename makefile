APP := tgmail-relay
BIN := $(APP)

# 远端主机别名（~/.ssh/config 里定义的 oci-fr；也可改成 user@ip）
REMOTE        ?= oci-fr
REMOTE_BIN    ?= /usr/local/bin/$(APP)
REMOTE_SVC    ?= $(APP)

# 目标架构（ARM64 服务器）；需要 x86_64 服务器时可临时覆盖：make deploy GOARCH=amd64
GOOS  ?= linux
GOARCH?= arm64

# 1) 本地编译
build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "-s -w" -o $(BIN) ./cmd/tgmail-relay

# 2) 本地运行（调试）
run:
	go run ./cmd/tgmail-relay

# 3) 一键部署：编译 -> 传到 /tmp -> sudo 安装到 /usr/local/bin -> 重启 systemd
deploy: build
	@tmp=/tmp/$(APP)-$$(date +%s); \
	echo ">> uploading to $(REMOTE):$$tmp"; \
	scp $(BIN) $(REMOTE):$$tmp; \
	echo ">> installing to $(REMOTE_BIN) and restarting $(REMOTE_SVC)"; \
	ssh $(REMOTE) "\
		sudo install -m755 $$tmp $(REMOTE_BIN) && \
		rm -f $$tmp && \
		sudo systemctl restart $(REMOTE_SVC) && \
		sudo systemctl status $(REMOTE_SVC) --no-pager --lines=3 \
	"
