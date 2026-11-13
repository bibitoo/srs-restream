# SRS 转推容器：常用操作
IMAGE      ?= srs-restream:1.0
CONTAINER  ?= srs-restream
SRS_SRC    ?= /home/sunz/AI/srs
DATA_DIR   ?= $(CURDIR)/data

.PHONY: help prepare build rebuild run stop rm logs shell test test-ui ctl build-ctl clean

help:
	@echo "make prepare   # 快照 SRS 源码到 build/srs-src"
	@echo "make build     # 构建镜像 $(IMAGE)"
	@echo "make rebuild   # 无缓存重建"
	@echo "make run       # 启动容器 $(CONTAINER)，端口 1935/8080/1985/1986"
	@echo "make stop      # 停止容器"
	@echo "make logs      # 跟踪日志"
	@echo "make shell     # 进入容器"
	@echo "make test      # 在容器内跑端到端自测"
	@echo "make test-ui   # 前端刷新逻辑测试（无需浏览器）"
	@echo "make build-ctl # 仅本地编译控制面（开发用）"

prepare:
	./scripts/prepare-srs-source.sh

build: prepare
	docker build -t $(IMAGE) .

rebuild: prepare
	docker build --no-cache -t $(IMAGE) .

run:
	mkdir -p $(DATA_DIR)
	docker rm -f $(CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(CONTAINER) \
		--ulimit nofile=65535:65535 \
		--add-host=host.docker.internal:host-gateway \
		-p 1935:1935 -p 8080:8080 -p 1985:1985 -p 1986:1986 \
		-v $(DATA_DIR):/data \
		--restart unless-stopped \
		$(IMAGE)
	@echo "控制台: http://127.0.0.1:1986/   推流: rtmp://127.0.0.1:1935/live/xxx"

stop:
	docker stop $(CONTAINER) || true

rm: stop
	docker rm $(CONTAINER) || true

logs:
	docker logs -f --tail=100 $(CONTAINER)

shell:
	docker exec -it $(CONTAINER) bash

test:
	docker cp scripts/smoke-test.sh $(CONTAINER):/tmp/smoke-test.sh
	docker exec $(CONTAINER) bash /tmp/smoke-test.sh

test-ui:
	node scripts/test-ui-refresh.js

build-ctl:
	cd control && CGO_ENABLED=0 go build -o srs-control .

clean:
	rm -rf build/srs-src control/srs-control
