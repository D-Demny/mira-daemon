#!/bin/sh
set -e
cd "$(dirname "$0")/sidecar"
RUSTFLAGS="-C linker=rust-lld -C target-feature=+crt-static" \
  cargo build --release --target armv7-unknown-linux-musleabihf
cp target/armv7-unknown-linux-musleabihf/release/iap2-sidecar ../iap2-sidecar-armv7
ls -la ../iap2-sidecar-armv7
