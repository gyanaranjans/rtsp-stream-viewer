#!/bin/sh
# Starts the demo RTSP server (unless disabled) alongside the backend.
set -e
if [ "${DEMO_STREAMS:-1}" = "1" ]; then
  mediamtx /app/mediamtx.yml &
fi
exec server
