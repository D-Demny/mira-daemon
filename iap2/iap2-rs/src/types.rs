use bytes::Bytes;

/// Link layer timing configuration
#[derive(Debug, Clone)]
pub struct LinkConfig {
    pub max_retries: u32,
    pub timeout_ms: u64,
    pub detect_to_syn_delay_ms: u64,
    pub syn_retry_delay_ms: u64,
    pub iap1_probe_timeout_ms: u64,
    pub initial_seq: u8,
}

impl Default for LinkConfig {
    fn default() -> Self {
        Self {
            max_retries: 30,
            timeout_ms: 1500,
            detect_to_syn_delay_ms: 100,
            syn_retry_delay_ms: 150,
            iap1_probe_timeout_ms: 150,
            initial_seq: 0x9D,
        }
    }
}

/// NowPlaying attributes configuration
#[derive(Debug, Clone)]
pub struct NowPlayingConfig {
    /// MediaItem attribute IDs to request (e.g., 0x01=Title, 0x04=Duration, 0x0C=Artist)
    pub media_item_attributes: Vec<u16>,
    /// Playback attribute IDs to request (e.g., 0x00=Status, 0x05=Shuffle)
    pub playback_attributes: Vec<u16>,
}

impl Default for NowPlayingConfig {
    fn default() -> Self {
        Self {
            media_item_attributes: vec![0x0001, 0x000C, 0x001A],
            playback_attributes: vec![0x0000, 0x0005, 0x0006, 0x0007],
        }
    }
}

/// HID configuration
#[derive(Debug, Clone)]
pub struct HidConfig {
    pub component_id: u16,
    pub vendor_id: u16,
    pub product_id: u16,
    pub report_descriptor: Vec<u8>,
}

impl Default for HidConfig {
    fn default() -> Self {
        Self {
            component_id: 0x14E9,
            vendor_id: 0x18D1,
            product_id: 0x4E26,
            report_descriptor: vec![
                0x05, 0x0C, 0x09, 0x01, 0xA1, 0x01, 0x15, 0x00, 0x25, 0x01, 0x75, 0x01, 0x95, 0x08,
                0x09, 0xB0, 0x09, 0xB1, 0x09, 0xB5, 0x09, 0xB6, 0x09, 0xB9, 0x09, 0xBC, 0x09, 0xE9,
                0x09, 0xEA, 0x81, 0x02, 0xC0,
            ],
        }
    }
}

/// File transfer timing configuration
#[derive(Debug, Clone)]
pub struct FileTransferConfig {
    pub transfer_timeout_secs: u64,
    pub stale_packet_grace_ms: u64,
}

impl Default for FileTransferConfig {
    fn default() -> Self {
        Self {
            transfer_timeout_secs: 30,
            stale_packet_grace_ms: 1000,
        }
    }
}

/// Connection-level timing configuration
#[derive(Debug, Clone)]
pub struct ConnectionConfig {
    pub keepalive_interval_secs: u64,
    pub challenge_timeout_ms: u64,
    pub ea_session_poll_secs: u64,
    pub cleanup_interval_secs: u64,
}

impl Default for ConnectionConfig {
    fn default() -> Self {
        Self {
            keepalive_interval_secs: 30,
            challenge_timeout_ms: 1800,
            ea_session_poll_secs: 60,
            cleanup_interval_secs: 2,
        }
    }
}

/// Power configuration for identification
#[derive(Debug, Clone, Default)]
pub struct PowerConfig {
    pub source_type: u8,
    pub max_current_ma: u16,
}

#[derive(Debug, Clone)]
pub struct DeviceIdentification {
    pub name: String,
    pub model_identifier: String,
    pub manufacturer: String,
    pub serial_number: String,
    pub firmware_version: String,
    pub hardware_version: String,
    pub ea_protocol_name: String,
    pub bundle_seed_id: Option<String>,
    pub current_language: String,
    pub supported_languages: Vec<String>,
    pub hid_components: Vec<HidComponent>,
    pub messages_sent: Vec<u16>,
    pub messages_received: Vec<u16>,
}

impl Default for DeviceIdentification {
    fn default() -> Self {
        Self {
            name: "iAP2 Accessory".to_string(),
            model_identifier: "GENERIC".to_string(),
            manufacturer: "Unknown".to_string(),
            serial_number: "00000000".to_string(),
            firmware_version: "1.0.0".to_string(),
            hardware_version: "1".to_string(),
            ea_protocol_name: "com.example.protocol".to_string(),
            bundle_seed_id: None,
            current_language: "en".to_string(),
            supported_languages: vec!["en".to_string()],
            hid_components: vec![],
            messages_sent: vec![0xEA02, 0xEA03],
            messages_received: vec![0xEA00, 0xEA01],
        }
    }
}

#[derive(Debug, Clone)]
pub struct HidComponent {
    pub id: u16,
    pub name: String,
    pub function: HidFunction,
    pub extra_data: Option<Vec<u8>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HidFunction {
    None,
    MediaPlaybackRemote,
    Custom(u8),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HidCommand {
    Play,
    Pause,
    PlayPause,
    Next,
    Previous,
    Shuffle,
    Repeat,
    VolumeUp,
    VolumeDown,
}

#[derive(Debug, Clone)]
#[allow(unused)]
pub struct SessionInfo {
    pub id: u8,
    pub protocol_id: u16,
    pub version: u8,
}

#[derive(Debug, Clone, Default)]
pub struct NowPlayingInfo {
    pub media_item: Option<NowPlayingMediaItem>,
    pub playback: Option<NowPlayingPlayback>,
}

#[derive(Debug, Clone, Default)]
pub struct NowPlayingMediaItem {
    pub title: Option<String>,
    pub artist: Option<String>,
    pub album: Option<String>,
    pub duration_ms: Option<u32>,
}

#[derive(Debug, Clone, Default)]
pub struct NowPlayingPlayback {
    pub status: Option<PlaybackStatus>,
    pub elapsed_ms: Option<u32>,
    pub shuffle_mode: Option<ShuffleMode>,
    pub repeat_mode: Option<RepeatMode>,
    pub app_name: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlaybackStatus {
    Stopped,
    Playing,
    Paused,
    SeekingForward,
    SeekingBackward,
}

impl PlaybackStatus {
    pub fn from_u8(value: u8) -> Self {
        match value {
            0 => Self::Stopped,
            1 => Self::Playing,
            2 => Self::Paused,
            3 => Self::SeekingForward,
            4 => Self::SeekingBackward,
            _ => Self::Stopped,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Stopped => "stopped",
            Self::Playing => "playing",
            Self::Paused => "paused",
            Self::SeekingForward => "seekingForward",
            Self::SeekingBackward => "seekingBackward",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ShuffleMode {
    Off,
    Songs,
    Albums,
}

impl ShuffleMode {
    pub fn from_u8(value: u8) -> Self {
        match value {
            0 => Self::Off,
            1 => Self::Songs,
            2 => Self::Albums,
            _ => Self::Off,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Off => "off",
            Self::Songs => "songs",
            Self::Albums => "albums",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RepeatMode {
    Off,
    One,
    All,
}

impl RepeatMode {
    pub fn from_u8(value: u8) -> Self {
        match value {
            0 => Self::Off,
            1 => Self::One,
            2 => Self::All,
            _ => Self::Off,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Off => "off",
            Self::One => "one",
            Self::All => "all",
        }
    }
}

#[derive(Debug, Clone)]
pub enum ConnectionEvent {
    LinkEstablished,

    AuthenticationStarted,

    AuthenticationSucceeded,

    AuthenticationFailed {
        reason: String,
    },

    IdentificationAccepted,

    IdentificationRejected,

    EaSessionStarted {
        session_id: u8,
    },

    EaSessionStopped {
        session_id: u8,
    },

    NowPlayingUpdate {
        update: NowPlayingInfo,
    },

    FileTransferComplete {
        transfer_id: u8,
        file_type: u16,
        data: Bytes,
    },

    FileTransferCorrupted {
        transfer_id: u8,
    },

    HidRemoteStarted,

    HidRemoteStopped,

    Error {
        error: String,
    },

    Disconnected,
}
