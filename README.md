# mira-daemon

The on-device daemon for **Mira**, a standalone Spotify Car Thing controller.

mira-daemon does not play audio. It runs in _observer_ mode, as a non-playable Spotify Connect device that watches what's playing on the user's other devices, exposes the state to the [mira-ui](https://github.com/mira-thing/mira-ui) frontend over HTTP and WebSocket, and forwards playback controls back to whichever device is active.

## What's different from upstream

- **Observer-only mode** - Reads ClusterUpdate messages from the dealer; controls are dispatched TO the active device.
- **Playback controls via the Connect-state protocol** (spclient), not the Web API.
- **HTTP/WebSocket API** for the frontend: `/observer/status`, `/player/*`, `/auth/*`, `/bluetooth/*`, `/network/status`, `/lyrics/*`, plus the `/events` WebSocket.
- **Device auth flow (RFC 8628)** with a QR code
- **Bluetooth manager** - BlueZ pairing agent, BT PAN setup with NAP-service detection
- **Synced lyrics provider** sourced from a primary souce and a fallback to LRCLIB.
- **Cold-boot hardening** - bounded `waitForClock`, retry-with-backoff session creation, online-state gate before HTTP attempts.

## Companion projects

- **[mira-ui](https://github.com/thing-project/thing-ui)** - React frontend that consumes this daemon's API
- **mira-firmware** - firmware build pipeline that bundles ui + daemon + kernel into a flashable image
- **[thing-releases](https://github.com/thing-project/thing-releases)** - prebuilt firmware images for end users

## Building

```bash
go build ./cmd/mira-daemon
```

For cross-compile to the Car Thing (armv6 userspace):

```bash
./crosscompile.sh armv6
```

## License

GPLv3, see [LICENSE](LICENSE). Inherited from upstream `devgianlu/go-librespot`. As a derivative work, all modifications in this repo are also GPLv3.

The original Spotify Connect protocol implementation and most of the audio, dealer, spclient, and login5 packages are authored by [devgianlu](https://github.com/devgianlu) and the go-librespot contributors. Full authorship history is preserved in the git log of this repo.

For upstream's docs (Docker setup, standalone usage, etc.), see the upstream README at https://github.com/devgianlu/go-librespot.

## Support

Mira is free and open source. If you'd like to support development, you can sponsor on [GitHub Sponsors](https://github.com/sponsors/MustakimK) or [Ko-fi](https://ko-fi.com/MustakimK). Questions and updates are on [Discord](https://discord.gg/SR2Pne7EPM).

## Attributions

mira-daemon is a heavily modified fork of [`devgianlu/go-librespot`](https://github.com/devgianlu/go-librespot) - the Spotify Connect protocol implementation and most of the dealer, spclient, and login5 packages are authored by devgianlu and the go-librespot contributors.

ios volume controls is built on [`usenocturne/iap2-rs`](https://github.com/usenocturne/iap2-rs), Nocturne's iAP2 protocol implementation, in [`iap2/`](iap2/).

> "Spotify" and "Car Thing" are trademarks of Spotify AB. This software is not affiliated with or endorsed by Spotify AB.
