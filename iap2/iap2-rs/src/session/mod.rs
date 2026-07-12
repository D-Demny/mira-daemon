mod control;
mod ea;
mod file_transfer;
mod hid;
mod now_playing;

pub use control::ControlSession;
pub use ea::{EaSession, EaSessionManager};
pub use file_transfer::{FileTransferHandler, FileTransferOutcome};
pub use hid::HidRemote;
pub use now_playing::NowPlayingParser;

#[allow(unused)]
pub const CONTROL_SESSION_ID: u8 = 0x00;
pub const FILE_TRANSFER_SESSION_ID: u8 = 0x01;
