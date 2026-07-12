use bytes::{BufMut, Bytes, BytesMut};
use std::collections::HashMap;
use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use crate::error::{Iap2Error, Result};
use crate::link::Iap2Link;
use crate::types::SessionInfo;

pub struct EaSession {
    pub session_id: u8,
    pub protocol: String,
    pub rx: mpsc::UnboundedReceiver<Bytes>,
    pub tx_to_iphone: mpsc::UnboundedSender<(u8, Bytes)>,
}

impl EaSession {
    pub fn send(&self, data: Bytes) -> Result<()> {
        self.tx_to_iphone
            .send((self.session_id, data))
            .map_err(|_| Iap2Error::ChannelClosed)
    }
}

pub struct EaSessionManager {
    sessions: HashMap<u8, SessionInfo>,
    session_channels: HashMap<u8, mpsc::UnboundedSender<Bytes>>,
    transfer_ids: HashMap<u8, u16>,
    ea_session_tx: mpsc::UnboundedSender<EaSession>,
    outgoing_tx: mpsc::UnboundedSender<(u8, Bytes)>,
    outgoing_rx: Option<mpsc::UnboundedReceiver<(u8, Bytes)>>,
    pending_ea_session: Option<String>,
}

impl EaSessionManager {
    pub fn new(ea_session_tx: mpsc::UnboundedSender<EaSession>) -> Self {
        let (outgoing_tx, outgoing_rx) = mpsc::unbounded_channel();

        EaSessionManager {
            sessions: HashMap::new(),
            session_channels: HashMap::new(),
            transfer_ids: HashMap::new(),
            ea_session_tx,
            outgoing_tx,
            outgoing_rx: Some(outgoing_rx),
            pending_ea_session: None,
        }
    }

    pub fn take_outgoing_rx(&mut self) -> Option<mpsc::UnboundedReceiver<(u8, Bytes)>> {
        self.outgoing_rx.take()
    }

    pub fn handle_session_started(&mut self, session_id: u8, protocol: String) -> Result<()> {
        info!(
            "EA session notification: ea_param_id={}, protocol={}",
            session_id, protocol
        );

        self.pending_ea_session = Some(protocol);

        Ok(())
    }

    fn register_session(&mut self, session_id: u8, protocol: String) -> Result<()> {
        info!(
            "Registering EA session: link_session_id={}, protocol={}",
            session_id, protocol
        );

        if self.sessions.contains_key(&session_id) {
            warn!("EA session {} already exists, replacing", session_id);
            self.sessions.remove(&session_id);
            self.session_channels.remove(&session_id);
        }

        self.sessions.insert(
            session_id,
            SessionInfo {
                id: session_id,
                protocol_id: 1,
                version: 1,
            },
        );

        let (tx_to_consumer, rx_from_lib) = mpsc::unbounded_channel();
        self.session_channels.insert(session_id, tx_to_consumer);

        let ea_session = EaSession {
            session_id,
            protocol: protocol.clone(),
            rx: rx_from_lib,
            tx_to_iphone: self.outgoing_tx.clone(),
        };

        self.ea_session_tx
            .send(ea_session)
            .map_err(|_| Iap2Error::ChannelClosed)?;

        Ok(())
    }

    pub fn handle_session_stopped(&mut self, session_id: u8) {
        info!("EA session stopped: id={}", session_id);
        self.sessions.remove(&session_id);
        self.session_channels.remove(&session_id);
        self.transfer_ids.remove(&session_id);
    }

    pub fn handle_incoming_data(&mut self, session_id: u8, payload: &[u8]) -> Result<()> {
        debug!(
            "Received EA data for session {}: {} bytes",
            session_id,
            payload.len()
        );

        let (transfer_id, ea_data) = if payload.len() >= 2 {
            let transfer_id = u16::from_be_bytes([payload[0], payload[1]]);
            (transfer_id, &payload[2..])
        } else if !payload.is_empty() {
            (0x0000u16, payload)
        } else {
            warn!("EA data packet empty");
            return Ok(());
        };

        self.transfer_ids.insert(session_id, transfer_id);

        if !self.session_channels.contains_key(&session_id) {
            if let Some(protocol) = self.pending_ea_session.take() {
                info!(
                    "First EA data arrived on link session {}, registering EA session now",
                    session_id
                );
                self.register_session(session_id, protocol)?;
            }
        }

        if let Some(tx) = self.session_channels.get(&session_id) {
            if tx.send(Bytes::copy_from_slice(ea_data)).is_err() {
                warn!("EA session {} channel closed, removing", session_id);
                self.session_channels.remove(&session_id);
            }
        } else {
            warn!("No channel for EA session {}", session_id);
        }

        Ok(())
    }

    pub async fn send_data(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        session_id: u8,
        data: Bytes,
    ) -> Result<()> {
        debug!(
            "Sending EA data to session {}: {} bytes",
            session_id,
            data.len()
        );

        let mut ea_datagram = BytesMut::new();

        let tx_id = self
            .transfer_ids
            .get(&session_id)
            .copied()
            .unwrap_or(0x0000);
        if tx_id == 0x0000 {
            warn!(
                "EA session {} has no known transfer ID yet; sending 0x0000",
                session_id
            );
        } else {
            debug!(
                "Using EA transfer ID 0x{:04X} for session {}",
                tx_id, session_id
            );
        }

        ea_datagram.put_u16(tx_id);
        ea_datagram.put_slice(&data);

        link.send_data(stream, session_id, ea_datagram.freeze())
            .await?;

        debug!("Successfully sent EA data packet to session {}", session_id);
        Ok(())
    }

    #[allow(unused)]
    pub fn active_sessions(&self) -> Vec<u8> {
        self.sessions.keys().copied().collect()
    }

    #[allow(unused)]
    pub fn has_session(&self, session_id: u8) -> bool {
        self.sessions.contains_key(&session_id)
    }
}
