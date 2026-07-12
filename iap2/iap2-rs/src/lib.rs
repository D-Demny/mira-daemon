mod auth;
mod connection;
mod error;
mod link;
mod mfi;
mod packet;
mod session;
mod types;

pub use connection::{connect, Iap2Config, Iap2Connection};
pub use error::{Iap2Error, Result};
pub use mfi::{MfiAuthProvider, MockMfiProvider};
pub use session::EaSession;
pub use types::{
    ConnectionConfig, ConnectionEvent, DeviceIdentification, FileTransferConfig, HidCommand,
    HidComponent, HidConfig, HidFunction, LinkConfig, NowPlayingConfig, NowPlayingInfo,
    NowPlayingMediaItem, NowPlayingPlayback, PlaybackStatus, PowerConfig, RepeatMode, ShuffleMode,
};
