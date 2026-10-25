FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/passway-server /passway-server
ENV PASSWAY_MODE=self-hosted
# WORKDIR creates /data owned by the image's nonroot user (BuildKit), so a
# new volume mounted there is writable. VOLUME alone would leave it root-owned.
WORKDIR /data
VOLUME /data
ENTRYPOINT ["/passway-server"]
