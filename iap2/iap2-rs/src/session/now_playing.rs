use tracing::{debug, warn};

use crate::types::{
    NowPlayingInfo, NowPlayingMediaItem, NowPlayingPlayback, PlaybackStatus, RepeatMode,
    ShuffleMode,
};

pub struct NowPlayingParser;

impl NowPlayingParser {
    pub fn parse(data: &[u8]) -> Option<NowPlayingInfo> {
        if data.is_empty() {
            debug!("NowPlayingUpdate with empty payload");
            return None;
        }

        let mut update = NowPlayingInfo::default();
        let mut has_relevant_data = false;

        for (param_id, payload) in Self::parse_parameters(data) {
            match param_id {
                0x0000 => {
                    if let Some(media_item) = Self::parse_media_item_group(payload) {
                        update.media_item = Some(media_item);
                        has_relevant_data = true;
                    }
                }
                0x0001 => {
                    if let Some(playback) = Self::parse_playback_group(payload) {
                        update.playback = Some(playback);
                        has_relevant_data = true;
                    }
                }
                _ => {
                    debug!(
                        "Unhandled NowPlayingUpdate parameter 0x{:04X} ({} bytes)",
                        param_id,
                        payload.len()
                    );
                }
            }
        }

        if has_relevant_data {
            Some(update)
        } else {
            None
        }
    }

    fn parse_parameters(data: &[u8]) -> Vec<(u16, &[u8])> {
        let mut params = Vec::new();
        let mut offset = 0;

        while offset + 4 <= data.len() {
            let length = u16::from_be_bytes([data[offset], data[offset + 1]]) as usize;
            if length < 4 {
                warn!("Parameter length too small: {}", length);
                break;
            }

            if offset + length > data.len() {
                warn!(
                    "Parameter length {} exceeds remaining payload {}",
                    length,
                    data.len() - offset
                );
                break;
            }

            let param_id = u16::from_be_bytes([data[offset + 2], data[offset + 3]]);
            let payload = &data[offset + 4..offset + length];
            params.push((param_id, payload));

            if length == 0 {
                break;
            }

            offset += length;
        }

        params
    }

    fn parse_media_item_group(data: &[u8]) -> Option<NowPlayingMediaItem> {
        let mut media = NowPlayingMediaItem::default();
        let mut has_data = false;

        for (param_id, payload) in Self::parse_parameters(data) {
            match param_id {
                0x0001 => {
                    if let Some(title) = Self::parse_utf8(payload) {
                        media.title = Some(title);
                        has_data = true;
                    }
                }
                0x0004 => {
                    if let Some(duration) = Self::parse_u32(payload) {
                        media.duration_ms = Some(duration);
                        has_data = true;
                    }
                }
                0x000C => {
                    if let Some(artist) = Self::parse_utf8(payload) {
                        media.artist = Some(artist);
                        has_data = true;
                    }
                }
                0x001A => {
                    if let Some(album) = Self::parse_utf8(payload) {
                        media.album = Some(album);
                        has_data = true;
                    }
                }
                _ => {}
            }
        }

        if has_data {
            Some(media)
        } else {
            None
        }
    }

    fn parse_playback_group(data: &[u8]) -> Option<NowPlayingPlayback> {
        let mut playback = NowPlayingPlayback::default();
        let mut has_data = false;

        for (param_id, payload) in Self::parse_parameters(data) {
            match param_id {
                0x0000 => {
                    if let Some(value) = payload.first().copied() {
                        playback.status = Some(PlaybackStatus::from_u8(value));
                        has_data = true;
                    }
                }
                0x0001 => {
                    if let Some(elapsed) = Self::parse_u32(payload) {
                        playback.elapsed_ms = Some(elapsed);
                        has_data = true;
                    }
                }
                0x0005 => {
                    if let Some(value) = payload.first().copied() {
                        playback.shuffle_mode = Some(ShuffleMode::from_u8(value));
                        has_data = true;
                    }
                }
                0x0006 => {
                    if let Some(value) = payload.first().copied() {
                        playback.repeat_mode = Some(RepeatMode::from_u8(value));
                        has_data = true;
                    }
                }
                0x0007 => {
                    if let Some(app_name) = Self::parse_utf8(payload) {
                        playback.app_name = Some(app_name);
                        has_data = true;
                    }
                }
                _ => {}
            }
        }

        if has_data {
            Some(playback)
        } else {
            None
        }
    }

    fn parse_utf8(payload: &[u8]) -> Option<String> {
        if payload.is_empty() {
            return None;
        }

        let trimmed = if let Some(pos) = payload.iter().position(|b| *b == 0) {
            &payload[..pos]
        } else {
            payload
        };

        if trimmed.is_empty() {
            return None;
        }

        let s = String::from_utf8_lossy(trimmed).to_string();

        if s.chars().all(|c| c == '\u{FFFD}') {
            debug!(
                "Skipping string with only replacement characters: {:?}",
                payload
            );
            return None;
        }

        if s.trim().is_empty() {
            return None;
        }

        Some(s)
    }

    fn parse_u32(payload: &[u8]) -> Option<u32> {
        if payload.len() < 4 {
            warn!("Expected u32 payload of 4 bytes, found {}", payload.len());
            return None;
        }

        Some(u32::from_be_bytes([
            payload[0], payload[1], payload[2], payload[3],
        ]))
    }
}
