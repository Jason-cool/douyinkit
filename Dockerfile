# syntax=docker/dockerfile:1

# ============================================================
#  douyinkit 抖音私信助手 —— Docker 镜像
#  运行阶段基于内网 harbor 的 alpine-chromium（Alpine/musl + 预装 Chromium），
#  容器自包含，不依赖宿主机 glibc，规避 CentOS 7（glibc 2.17）跑不动
#  rod 自动下载的新版 Chromium（GLIBC_2.18/2.25 not found）的问题。
#  前提：构建机能访问并登录 harbor.chaoniu.top:4433。
# ============================================================

# ---------- 1) 编译阶段：产出静态二进制 ----------
FROM harbor.chaoniu.top:4433/sushou/golang:1.21.5-alpine AS builder

ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GOPROXY=https://goproxy.cn,direct

WORKDIR /src

# 先拷依赖清单，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷源码并编译（本项目入口是 ./cmd/demo，不是根目录 main.go）
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/demo ./cmd/demo

# ---------- 2) 运行阶段：Alpine + Chromium ----------
FROM harbor.chaoniu.top:4433/sushou/alpine-chromium:latest

ENV TZ=Asia/Shanghai \
    LANG=C.UTF-8

# 关键补丁：
#  - font-noto-cjk：中文字体，否则抖音中文昵称/消息渲染成方块，影响识别（参考镜像只带了 emoji）
#  - tzdata + 时区：免打扰时段/消息时间判断依赖本地时间，必须 Asia/Shanghai
RUN set -ex \
    && apk add --no-cache font-noto-cjk font-noto-emoji tzdata \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && fc-cache -f

WORKDIR /app

# 只拷编译产物（不把源码带进运行镜像）
COPY --from=builder /out/demo /app/demo

# 数据目录（登录态/日志/冷却记录）——用卷挂载持久化，勿写进镜像
RUN mkdir -p /app/douyinkit-data
VOLUME ["/app/douyinkit-data"]

# 二维码 HTTP 服务端口
EXPOSE 6060

ENTRYPOINT ["/app/demo"]

# 默认：走容器内 /usr/bin/chromium（-channel chromium），启动即预热（常驻），
# 持续自动回复（真实发送）。想先干跑观察，去掉 "-send","-loop" 即可（见 docker-compose.yml）。
CMD ["-base", "/app/douyinkit-data", "-addr", ":6060", "-channel", "chromium", "-preheat", "-send", "-loop"]
