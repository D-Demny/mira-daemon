use std::sync::Arc;

use bluer::rfcomm::Stream;
use bytes::Bytes;
use tokio::sync::{mpsc, Mutex};
use tokio::time::{Duration, Instant};
use tracing::{debug, error, info, warn};

use crate::auth::Iap2Auth;
use crate::error::{Iap2Error, Result};
use crate::link::Iap2Link;
use crate::mfi::MfiAuthProvider;
use crate::session::{
    ControlSession, EaSession, EaSessionManager, FileTransferHandler, FileTransferOutcome,
    HidRemote, NowPlayingParser, FILE_TRANSFER_SESSION_ID,
};
use crate::types::{
    ConnectionConfig, ConnectionEvent, DeviceIdentification, FileTransferConfig, HidCommand,
    HidConfig, LinkConfig, NowPlayingConfig, PowerConfig,
};

pub struct Iap2Config {
    pub identification: DeviceIdentification,
    pub mfi_provider: Arc<dyn MfiAuthProvider>,
    pub enable_now_playing: bool,
    pub enable_hid: bool,
    pub link_config: LinkConfig,
    pub now_playing_config: NowPlayingConfig,
    pub hid_config: HidConfig,
    pub file_transfer_config: FileTransferConfig,
    pub connection_config: ConnectionConfig,
    pub power_config: PowerConfig,
}

pub struct Iap2Connection {
    pub events: mpsc::UnboundedReceiver<ConnectionEvent>,
    pub ea_sessions: mpsc::UnboundedReceiver<EaSession>,
    hid_tx: mpsc::UnboundedSender<HidCommand>,
    app_launch_tx: mpsc::UnboundedSender<String>,
    running: Arc<Mutex<bool>>,
}

impl Iap2Connection {
    pub async fn is_running(&self) -> bool {
        *self.running.lock().await
    }

    pub fn send_hid_command(&self, command: HidCommand) -> Result<()> {
        self.hid_tx
            .send(command)
            .map_err(|_| Iap2Error::ChannelClosed)
    }

    pub fn request_app_launch(&self, bundle_id: String) -> Result<()> {
        self.app_launch_tx
            .send(bundle_id)
            .map_err(|_| Iap2Error::ChannelClosed)
    }
}

pub async fn connect(stream: Stream, config: Iap2Config) -> Result<Iap2Connection> {
    let (event_tx, event_rx) = mpsc::unbounded_channel();
    let (ea_session_tx, ea_session_rx) = mpsc::unbounded_channel();
    let (hid_tx, hid_rx) = mpsc::unbounded_channel();
    let (app_launch_tx, app_launch_rx) = mpsc::unbounded_channel();

    let running = Arc::new(Mutex::new(true));
    let running_clone = running.clone();

    tokio::spawn(async move {
        let result = run_connection(
            stream,
            config,
            event_tx.clone(),
            ea_session_tx,
            hid_rx,
            app_launch_rx,
            running_clone.clone(),
        )
        .await;

        if let Err(e) = result {
            error!("iAP2 connection error: {}", e);
            let _ = event_tx.send(ConnectionEvent::Error {
                error: e.to_string(),
            });
        }

        let _ = event_tx.send(ConnectionEvent::Disconnected);
        *running_clone.lock().await = false;
    });

    Ok(Iap2Connection {
        events: event_rx,
        ea_sessions: ea_session_rx,
        hid_tx,
        app_launch_tx,
        running,
    })
}

async fn run_connection(
    stream: Stream,
    config: Iap2Config,
    event_tx: mpsc::UnboundedSender<ConnectionEvent>,
    ea_session_tx: mpsc::UnboundedSender<EaSession>,
    mut hid_rx: mpsc::UnboundedReceiver<HidCommand>,
    mut app_launch_rx: mpsc::UnboundedReceiver<String>,
    running: Arc<Mutex<bool>>,
) -> Result<()> {
    let stream = Arc::new(Mutex::new(stream));
    let mut link = Iap2Link::new(config.link_config.clone());

    {
        let mut stream_guard = stream.lock().await;
        link.negotiate(&mut stream_guard).await?;
    }
    let _ = event_tx.send(ConnectionEvent::LinkEstablished);
    info!("Link negotiation complete, waiting for authentication");

    let mut auth = Iap2Auth::new(config.mfi_provider);
    let control_session_id = handle_authentication(
        &mut link,
        &stream,
        &mut auth,
        &event_tx,
        &config.connection_config,
    )
    .await?;

    let control = ControlSession::new(
        config.identification,
        control_session_id,
        config.now_playing_config.clone(),
        config.power_config.clone(),
    );
    let mut ea_manager = EaSessionManager::new(ea_session_tx);
    let mut outgoing_rx = ea_manager.take_outgoing_rx();
    let mut file_transfer = FileTransferHandler::new(config.file_transfer_config.clone());
    let mut hid_remote = HidRemote::new(control_session_id, config.hid_config.clone());
    let mut now_playing_active = false;

    let keepalive_interval = Duration::from_secs(config.connection_config.keepalive_interval_secs);
    let cleanup_interval = Duration::from_secs(config.connection_config.cleanup_interval_secs);
    let ea_poll_duration = Duration::from_secs(config.connection_config.ea_session_poll_secs);
    let mut last_keepalive = Instant::now();

    while *running.lock().await {
        tokio::select! {
            result = async {
                let mut stream_guard = stream.lock().await;
                link.receive_data(&mut stream_guard).await
            } => {
                match result {
                    Ok(packet) => {
                        if let Some(sid) = packet.session_id {
                            let mut stream_guard = stream.lock().await;

                            if sid == control_session_id {
                                handle_control_message(
                                    &mut link,
                                    &mut stream_guard,
                                    &control,
                                    &mut ea_manager,
                                    &mut hid_remote,
                                    &mut now_playing_active,
                                    &packet.payload,
                                    &event_tx,
                                    config.enable_now_playing,
                                    config.enable_hid,
                                ).await?;
                            } else if sid == FILE_TRANSFER_SESSION_ID {
                                handle_file_transfer(
                                    &mut link,
                                    &mut stream_guard,
                                    &mut file_transfer,
                                    &packet.payload,
                                    &event_tx,
                                ).await?;
                            } else {
                                ea_manager.handle_incoming_data(sid, &packet.payload)?;
                            }
                        }
                        last_keepalive = Instant::now();
                    }
                    Err(e) => {
                        error!("Error receiving data: {}", e);
                        return Err(e);
                    }
                }
            }

            data = async {
                match &mut outgoing_rx {
                    Some(rx) => rx.recv().await,
                    None => {
                        tokio::time::sleep(ea_poll_duration).await;
                        None
                    }
                }
            } => {
                if let Some((session_id, data)) = data {
                    let mut stream_guard = stream.lock().await;
                    if let Err(e) = ea_manager.send_data(&mut link, &mut stream_guard, session_id, data).await {
                        error!("Failed to send EA data: {}", e);
                    }
                }
            }

            cmd = hid_rx.recv() => {
                if let Some(command) = cmd {
                    let mut stream_guard = stream.lock().await;
                    if let Err(e) = hid_remote.send_command(&mut link, &mut stream_guard, command).await {
                        error!("Failed to send HID command: {}", e);
                    }
                }
            }

            bundle_id = app_launch_rx.recv() => {
                if let Some(bundle_id) = bundle_id {
                    let mut stream_guard = stream.lock().await;
                    if let Err(e) = control.send_app_launch_request(&mut link, &mut stream_guard, &bundle_id).await {
                        error!("Failed to send app launch request: {}", e);
                    }
                }
            }

            _ = tokio::time::sleep(cleanup_interval) => {
                let timed_out = file_transfer.cleanup_stale();
                for transfer_id in timed_out {
                    warn!(
                        "File transfer 0x{:02X} timed out, notifying as corrupted",
                        transfer_id
                    );
                    let _ = event_tx.send(ConnectionEvent::FileTransferCorrupted { transfer_id });
                }

                if last_keepalive.elapsed() > keepalive_interval {
                    let mut stream_guard = stream.lock().await;
                    if let Err(e) = control.send_keepalive(&mut link, &mut stream_guard).await {
                        error!("Failed to send keepalive: {}", e);
                        return Err(e);
                    }
                    last_keepalive = Instant::now();
                }
            }
        }
    }

    if now_playing_active {
        let mut stream_guard = stream.lock().await;
        let _ = control
            .send_stop_now_playing_updates(&mut link, &mut stream_guard)
            .await;
    }
    if hid_remote.is_started() {
        let mut stream_guard = stream.lock().await;
        let _ = hid_remote.send_stop(&mut link, &mut stream_guard).await;
    }

    info!("iAP2 connection ended");
    Ok(())
}

async fn handle_authentication(
    link: &mut Iap2Link,
    stream: &Arc<Mutex<Stream>>,
    auth: &mut Iap2Auth,
    event_tx: &mpsc::UnboundedSender<ConnectionEvent>,
    connection_config: &ConnectionConfig,
) -> Result<u8> {
    let mut control_session_id = None;
    let challenge_timeout = Duration::from_millis(connection_config.challenge_timeout_ms);

    loop {
        let mut stream_guard = stream.lock().await;
        let packet = link.receive_data(&mut stream_guard).await?;
        drop(stream_guard);

        if let Some(sid) = packet.session_id {
            if control_session_id.is_none() {
                control_session_id = Some(sid);
                info!("Discovered control session ID: 0x{:02X}", sid);
            }
        }

        if packet.payload.len() < 6 {
            warn!("Control message too short: {} bytes", packet.payload.len());
            continue;
        }

        if packet.payload[0] != 0x40 || packet.payload[1] != 0x40 {
            continue;
        }

        let msg_len = u16::from_be_bytes([packet.payload[2], packet.payload[3]]) as usize;
        let msg_id = u16::from_be_bytes([packet.payload[4], packet.payload[5]]);

        if msg_len < 6 || msg_len > packet.payload.len() {
            continue;
        }

        debug!("Received auth control message 0x{:04X}", msg_id);

        match msg_id {
            0xAA00 => {
                // RequestAuthenticationCertificate
                info!("iPhone requested authentication certificate");
                let _ = event_tx.send(ConnectionEvent::AuthenticationStarted);

                let mut stream_guard = stream.lock().await;
                let sid = control_session_id.unwrap_or(0x0A);
                auth.handle_certificate_request(link, &mut stream_guard, sid)
                    .await?;
            }
            0xAA02 => {
                // RequestAuthenticationChallengeResponse
                info!("iPhone sent authentication challenge");
                let mut stream_guard = stream.lock().await;
                let sid = control_session_id.unwrap_or(0x0A);

                let send_resp = async {
                    auth.handle_challenge_request(
                        link,
                        &mut stream_guard,
                        sid,
                        &packet.payload[6..msg_len],
                    )
                    .await
                };

                match tokio::time::timeout(challenge_timeout, send_resp).await {
                    Ok(r) => r?,
                    Err(_) => return Err(Iap2Error::Timeout),
                }
            }
            0xAA05 => {
                // AuthenticationSucceeded
                info!("Authentication succeeded!");
                let _ = event_tx.send(ConnectionEvent::AuthenticationSucceeded);
                return Ok(control_session_id.unwrap_or(0x0A));
            }
            0xAA04 => {
                // AuthenticationFailed
                error!("Authentication failed!");
                let _ = event_tx.send(ConnectionEvent::AuthenticationFailed {
                    reason: "iPhone rejected authentication".to_string(),
                });
                return Err(Iap2Error::AuthenticationFailed(
                    "iPhone rejected authentication".to_string(),
                ));
            }
            _ => {
                warn!("Unhandled auth control message 0x{:04X}", msg_id);
            }
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn handle_control_message(
    link: &mut Iap2Link,
    stream: &mut Stream,
    control: &ControlSession,
    ea_manager: &mut EaSessionManager,
    hid_remote: &mut HidRemote,
    now_playing_active: &mut bool,
    payload: &[u8],
    event_tx: &mpsc::UnboundedSender<ConnectionEvent>,
    enable_now_playing: bool,
    enable_hid: bool,
) -> Result<()> {
    if payload.len() < 6 {
        return Ok(());
    }

    if payload[0] != 0x40 || payload[1] != 0x40 {
        return Ok(());
    }

    let msg_len = u16::from_be_bytes([payload[2], payload[3]]) as usize;
    if msg_len < 6 || msg_len > payload.len() {
        return Ok(());
    }

    let msg_id = u16::from_be_bytes([payload[4], payload[5]]);
    info!(
        "Received control message 0x{:04X}, payload_len={}, msg_len={}",
        msg_id,
        payload.len(),
        msg_len
    );

    match msg_id {
        0x1D00 => {
            // StartIdentification
            info!("Received StartIdentification");
            control.send_identification(link, stream).await?;
        }
        0x1D02 => {
            // IdentificationAccepted
            let _ = event_tx.send(ConnectionEvent::IdentificationAccepted);
            if control.has_ea_protocol() {
                info!("Identification accepted, requesting EA session");
                control.send_ea_session_request(link, stream, 0x00).await?;
            } else {
                info!("Identification accepted (no EA protocol declared)");
            }
        }
        0x1D03 => {
            // IdentificationRejected
            warn!("Identification rejected by device");
            let _ = event_tx.send(ConnectionEvent::IdentificationRejected);
        }
        0xEA00 => {
            // StartExternalAccessoryProtocolSession response
            info!(
                "EA session started by device, payload len={}",
                payload.len()
            );

            // - 2 bytes: param length
            // - 2 bytes: param id (0x0000 for session ID)
            // - 1 byte: session ID value
            let params = &payload[6..msg_len];
            if params.len() >= 5 {
                let param_len = u16::from_be_bytes([params[0], params[1]]) as usize;
                let param_id = u16::from_be_bytes([params[2], params[3]]);

                info!(
                    "EA session response: param_len={}, param_id=0x{:04X}",
                    param_len, param_id
                );

                if param_id == 0x0000 && params.len() >= 5 {
                    let session_id = params[4];
                    info!("EA session ID from device: {}", session_id);

                    ea_manager
                        .handle_session_started(session_id, "com.usenocturne.daemon".to_string())?;
                    let _ = event_tx.send(ConnectionEvent::EaSessionStarted { session_id });

                    if enable_now_playing && !*now_playing_active {
                        control.send_start_now_playing_updates(link, stream).await?;
                        *now_playing_active = true;
                    }
                    if enable_hid && hid_remote.ensure_started(link, stream).await? {
                        let _ = event_tx.send(ConnectionEvent::HidRemoteStarted);
                    }
                } else {
                    warn!(
                        "Invalid EA session response format: param_id=0x{:04X}, params_len={}",
                        param_id,
                        params.len()
                    );
                }
            } else {
                warn!("EA session response too short: params_len={}", params.len());
            }
        }
        0xEA01 => {
            // StopExternalAccessoryProtocolSession
            info!("EA session stopped by device");
            if payload.len() >= 11 {
                let session_id = payload[10];
                ea_manager.handle_session_stopped(session_id);
                let _ = event_tx.send(ConnectionEvent::EaSessionStopped { session_id });
            }

            if hid_remote.is_started() {
                hid_remote.send_stop(link, stream).await?;
                let _ = event_tx.send(ConnectionEvent::HidRemoteStopped);
            }
        }
        0x5001 => {
            // NowPlayingUpdate
            debug!("Received NowPlayingUpdate");
            if let Some(update) = NowPlayingParser::parse(&payload[6..msg_len]) {
                let _ = event_tx.send(ConnectionEvent::NowPlayingUpdate { update });
            }
        }
        0x4158 => {
            // StatusUpdate response
            debug!("Received StatusUpdate response");
        }
        _ => {
            debug!("Unhandled control message 0x{:04X}", msg_id);
        }
    }

    Ok(())
}

async fn handle_file_transfer(
    link: &mut Iap2Link,
    stream: &mut Stream,
    handler: &mut FileTransferHandler,
    payload: &Bytes,
    event_tx: &mpsc::UnboundedSender<ConnectionEvent>,
) -> Result<()> {
    match handler
        .handle_datagram(link, stream, FILE_TRANSFER_SESSION_ID, payload)
        .await?
    {
        FileTransferOutcome::Pending => {}
        FileTransferOutcome::Completed {
            transfer_id,
            file_type,
            data,
        } => {
            info!(
                "File transfer complete: id=0x{:02X}, type=0x{:04X}, {} bytes",
                transfer_id,
                file_type,
                data.len()
            );
            let _ = event_tx.send(ConnectionEvent::FileTransferComplete {
                transfer_id,
                file_type,
                data,
            });
        }
        FileTransferOutcome::Corrupted {
            transfer_id,
            reason,
        } => {
            warn!(
                "File transfer corrupted: id=0x{:02X}, reason={}",
                transfer_id, reason
            );
            let _ = event_tx.send(ConnectionEvent::FileTransferCorrupted { transfer_id });
        }
    }
    Ok(())
}
