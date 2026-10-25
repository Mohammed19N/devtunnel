FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/passway-server /passway-server
ENV PASSWAY_MODE=self-hosted
VOLUME /data
ENTRYPOINT ["/passway-server"]
