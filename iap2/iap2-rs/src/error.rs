use thiserror::Error;

#[derive(Error, Debug)]
pub enum Iap2Error {
    #[error("Protocol error: {0}")]
    Protocol(String),

    #[error("Authentication failed: {0}")]
    AuthenticationFailed(String),

    #[error("MFi error: {0}")]
    Mfi(String),

    #[error("Invalid packet: {0}")]
    InvalidPacket(String),

    #[error("IO error: {0}")]
    Io(#[from] std::io::Error),

    #[error("Timeout")]
    Timeout,

    #[error("Session not found: {0}")]
    SessionNotFound(u8),

    #[error("Channel closed")]
    ChannelClosed,

    #[error("Connection closed")]
    ConnectionClosed,
}

pub type Result<T> = std::result::Result<T, Iap2Error>;
