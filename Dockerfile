FROM ghcr.io/vilsol/ffmpeg-alpine:latest as ffmpeg


FROM golang:latest as build

WORKDIR /app

COPY * .

RUN go mod download && \
	go build -o transcoder-go

FROM alpine:edge

# ffmpeg
COPY --from=ffmpeg /root/bin/ffmpeg /bin/ffmpeg
COPY --from=ffmpeg /root/bin/ffprobe /bin/ffprobe

# x265
COPY --from=ffmpeg /usr/local/ /usr/local/

RUN apk add --no-cache \
	libtheora \
	libvorbis \
	x264-libs \
	fdk-aac \
	lame \
	opus \
	libvpx \
	libstdc++ \
	numactl \
	nasm

COPY --from=build /app/transcoder-go /transcoder

ENTRYPOINT ["/transcoder"]
