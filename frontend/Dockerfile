# syntax=docker/dockerfile:1
#
# swarm-net dashboard image: nginx serves the static files in this directory
# and reverse-proxies /api/, /healthz and /ws to the control center.
#
#   docker build -t swarm-net/frontend frontend
#   docker run -p 127.0.0.1:8080:8080 -e CONTROL_CENTER_UPSTREAM=cc:8080 swarm-net/frontend
#
# No build stage: the dashboard is vanilla HTML/CSS/JS, copied as-is.

ARG NGINX_VERSION=1.31-alpine
FROM nginx:${NGINX_VERSION}

# CONTROL_CENTER_UPSTREAM: host:port of the control center's HTTP server,
# rendered into the config by the image's envsubst step
# (/etc/nginx/templates -> /etc/nginx/conf.d).
# NGINX_ENTRYPOINT_LOCAL_RESOLVERS: export the container's nameservers as
# NGINX_LOCAL_RESOLVERS for the template's `resolver` line.
ENV CONTROL_CENTER_UPSTREAM=control-center:8080 \
    NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1 \
    NGINX_ENTRYPOINT_QUIET_LOGS=1

# Run as the image's unprivileged `nginx` user (uid 101):
#  - drop the `user` directive (only meaningful when the master is root);
#  - move the pid file to /tmp;
#  - let uid 101 write the cache/temp dirs and the rendered conf.d;
#  - listen on 8080, not 80 (see nginx.conf.template).
# The stock default server and welcome page are removed.
RUN sed -i -e '/^user /d' -e 's|^pid .*|pid /tmp/nginx.pid;|' /etc/nginx/nginx.conf \
    && rm -f /etc/nginx/conf.d/default.conf \
    && rm -rf /usr/share/nginx/html/* \
    && chown -R nginx:nginx /var/cache/nginx /etc/nginx/conf.d

COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --chmod=0444 index.html app.js style.css /usr/share/nginx/html/

USER 101:101
EXPOSE 8080

# The base image stops on SIGQUIT (graceful: waits for open connections,
# which for a WebSocket means until the Compose grace period expires).
# SIGTERM is nginx's fast shutdown, right for a stateless proxy: `docker stop`
# returns at once and the dashboard's own reconnect loop takes over.
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=2s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/frontend-healthz || exit 1
