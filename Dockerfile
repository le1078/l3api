# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
# 国内云机拉不动 proxy.golang.org：默认走 goproxy.cn 镜像；
# 海外构建可用 --build-arg GOPROXY=https://proxy.golang.org,direct 覆盖。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/l3api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 # 构建期质量门：vet + 单测失败直接让 docker build 失败，问题不带进镜像。
 && go vet ./... \
 && go test ./... -count=1 -timeout 300s

FROM alpine:3.20
# 国内云机 dl-cdn.alpinelinux.org 拉不动：换阿里云镜像；
# 海外构建可用 --build-arg APK_MIRROR=dl-cdn.alpinelinux.org 覆盖。
ARG APK_MIRROR=mirrors.aliyun.com
RUN sed -i "s/dl-cdn.alpinelinux.org/${APK_MIRROR}/g" /etc/apk/repositories
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/l3api /app/l3api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带任何默认 config.json：缺失时程序启动走 WriteDefault 自动生成
# 含随机强 api_key 的配置（0600 权限）。生产由挂载卷 /app/config.json 覆盖。
# 切勿把 config.example.json 拷成 config.json——其 api_key=test_key 是公开值。
USER app
EXPOSE 7863
# HEALTHCHECK 打 /livez（进程恒活探针）：无账号/全池冷却时 /healthz 返 503，
# 用它做 Docker 健康检查会让容器被误判 unhealthy 反复重启；/healthz 留给 LB。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/livez || exit 1
ENTRYPOINT ["/app/l3api", "-config", "/app/config.json"]