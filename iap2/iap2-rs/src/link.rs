use bytes::{Buf, BytesMut};
use crate::error::{Iap2Error, Result};
use crate::packet::{Iap2Packet, PacketType};
use crate::types::LinkConfig;
use bluer::rfcomm::Stream;
use std::collections::{BTreeMap, VecDeque};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::time::{timeout, Duration};
use tracing::{debug, info, warn};

pub struct Iap2Link {
    seq: u8,
    ack: u8,
    state: LinkState,
    config: LinkConfig,
    pending: VecDeque<Iap2Packet>,
    rx_buffer: BTreeMap<u8, Iap2Packet>,
    read_buf: BytesMut,
}

#[derive(Debug, Clone, Copy, PartialEq)]
enum LinkState {
    Idle,
    SynSent,
    Established,
}

impl Iap2Link {
    pub fn new(config: LinkConfig) -> Self {
        Iap2Link {
            seq: 0x10,
            ack: 0x00,
            state: LinkState::Idle,
            config,
            pending: VecDeque::new(),
            rx_buffer: BTreeMap::new(),
            read_buf: BytesMut::with_capacity(8192),
        }
    }

    #[allow(unused)]
    pub fn is_established(&self) -> bool {
        self.state == LinkState::Established
    }

    pub async fn negotiate(&mut self, stream: &mut Stream) -> Result<()> {
        info!("Starting iAP2 link negotiation");

        let _ = self.try_iap1_probe(stream).await;

        self.send_detect(stream).await?;
        tokio::time::sleep(Duration::from_millis(self.config.detect_to_syn_delay_ms)).await;

        self.send_link_sync(stream).await?;
        let mut attempt = 0;
        loop {
            match self.wait_for_syn_ack(stream).await {
                Ok(()) => break,
                Err(e) => {
                    attempt += 1;
                    if attempt > self.config.max_retries {
                        return Err(e);
                    }
                    warn!(
                        "No SYN-ACK, retrying SYN (attempt {}/{})",
                        attempt, self.config.max_retries
                    );
                    tokio::time::sleep(Duration::from_millis(self.config.syn_retry_delay_ms)).await;
                    self.resend_link_sync(stream).await?;
                }
            }
        }

        self.send_ack(stream).await?;

        self.state = LinkState::Established;
        info!("iAP2 link established");

        Ok(())
    }

    async fn try_iap1_probe(&mut self, stream: &mut Stream) -> Result<()> {
        let iap1_check = [0xFF, 0x55, 0x02, 0x00, 0xEE, 0x10];
        stream.write_all(&iap1_check).await?;
        stream.flush().await?;
        let mut buf = [0u8; 6];
        let _ = timeout(
            Duration::from_millis(self.config.iap1_probe_timeout_ms),
            stream.read_exact(&mut buf),
        )
        .await;
        Ok(())
    }

    async fn send_detect(&mut self, stream: &mut Stream) -> Result<()> {
        info!("Sending iAP2 DETECT");
        let packet = Iap2Packet::detect();
        self.write_packet(stream, &packet).await?;
        Ok(())
    }

    async fn send_link_sync(&mut self, stream: &mut Stream) -> Result<()> {
        info!("Sending SYN packet to establish iAP2 link");

        self.seq = self.config.initial_seq;

        let packet = Iap2Packet::syn(self.seq);
        self.write_packet(stream, &packet).await?;

        self.state = LinkState::SynSent;
        info!("Sent SYN packet with seq=0x{:02X}", self.seq);
        Ok(())
    }

    async fn resend_link_sync(&mut self, stream: &mut Stream) -> Result<()> {
        let packet = Iap2Packet::syn(self.seq);
        self.write_packet(stream, &packet).await?;
        debug!("Resent SYN packet with seq=0x{:02X}", self.seq);
        Ok(())
    }

    async fn wait_for_syn_ack(&mut self, stream: &mut Stream) -> Result<()> {
        debug!("Waiting for SYN-ACK packet from iPhone");

        let packet = self.read_packet_with_timeout(stream).await?;

        if packet.control.packet_type != PacketType::SynAck {
            return Err(Iap2Error::Protocol(format!(
                "Expected SYN-ACK, got {:?}",
                packet.control.packet_type
            )));
        }

        if packet.ack != self.seq {
            return Err(Iap2Error::Protocol(format!(
                "SYN-ACK has wrong ack: expected 0x{:02X}, got 0x{:02X}",
                self.seq, packet.ack
            )));
        }

        self.ack = packet.seq;

        debug!(
            "Received SYN-ACK with seq=0x{:02X}, ack=0x{:02X}",
            packet.seq, packet.ack
        );
        Ok(())
    }

    async fn send_ack(&mut self, stream: &mut Stream) -> Result<()> {
        debug!("Sending ACK packet to complete handshake");

        self.seq = self.seq.wrapping_add(1);

        let packet = Iap2Packet::ack(self.seq, self.ack);
        self.write_packet(stream, &packet).await?;

        debug!(
            "Sent ACK with seq=0x{:02X}, ack=0x{:02X}",
            self.seq, self.ack
        );
        Ok(())
    }

    pub async fn receive_data(&mut self, stream: &mut Stream) -> Result<Iap2Packet> {
        if let Some(pkt) = self.pending.pop_front() {
            debug!(
                "Delivering pending control message with seq=0x{:02X}",
                pkt.seq
            );
            return Ok(pkt);
        }

        loop {
            if !self.rx_buffer.is_empty() {
                let expected_seq = self.ack.wrapping_add(1);
                if let Some(buffered) = self.rx_buffer.remove(&expected_seq) {
                    debug!(
                        "Delivering buffered packet with seq=0x{:02X} (was out-of-order)",
                        expected_seq
                    );
                    self.ack = buffered.seq;
                    let ack = if let Some(s) = buffered.session_id {
                        Iap2Packet::ack_with_session(self.seq, self.ack, s)
                    } else {
                        Iap2Packet::ack(self.seq, self.ack)
                    };
                    self.write_packet(stream, &ack).await?;
                    return Ok(buffered);
                }
            }

            let packet = self.read_packet(stream).await?;
            let has_payload = !packet.payload.is_empty();

            match packet.control.packet_type {
                PacketType::Data if has_payload => {
                    let expected_seq = self.ack.wrapping_add(1);

                    if packet.seq == expected_seq {
                        self.ack = packet.seq;
                        let ack = Iap2Packet::ack(self.seq, self.ack);
                        self.write_packet(stream, &ack).await?;
                        return Ok(packet);
                    } else if self.is_seq_after(packet.seq, expected_seq) {
                        warn!(
                            "Out-of-order packet: expected seq=0x{:02X}, got seq=0x{:02X}, buffering",
                            expected_seq, packet.seq
                        );
                        self.rx_buffer.insert(packet.seq, packet.clone());

                        let ack = Iap2Packet::ack(self.seq, self.ack);
                        self.write_packet(stream, &ack).await?;
                        continue;
                    } else {
                        warn!(
                            "Duplicate/old packet: expected seq=0x{:02X}, got seq=0x{:02X}, dropping",
                            expected_seq, packet.seq
                        );
                        let ack = Iap2Packet::ack(self.seq, self.ack);
                        self.write_packet(stream, &ack).await?;
                        continue;
                    }
                }
                PacketType::Ack if has_payload => {
                    let expected_seq = self.ack.wrapping_add(1);

                    if packet.seq == expected_seq || packet.seq == self.ack {
                        self.ack = packet.seq;
                        let ack = if let Some(s) = packet.session_id {
                            Iap2Packet::ack_with_session(self.seq, self.ack, s)
                        } else {
                            Iap2Packet::ack(self.seq, self.ack)
                        };
                        self.write_packet(stream, &ack).await?;
                        return Ok(packet);
                    } else if self.is_seq_after(packet.seq, expected_seq) {
                        warn!(
                            "Out-of-order ACK packet: expected seq=0x{:02X}, got seq=0x{:02X}, buffering",
                            expected_seq, packet.seq
                        );
                        self.rx_buffer.insert(packet.seq, packet.clone());
                        continue;
                    } else {
                        debug!(
                            "Duplicate/old ACK packet with seq=0x{:02X}, dropping",
                            packet.seq
                        );
                        continue;
                    }
                }
                PacketType::Ack => {
                    debug!("Received ACK packet without payload");
                    continue;
                }
                _ => {
                    warn!("Unexpected packet type: {:?}", packet.control.packet_type);
                    continue;
                }
            }
        }
    }

    pub async fn send_data(
        &mut self,
        stream: &mut Stream,
        session_id: u8,
        payload: bytes::Bytes,
    ) -> Result<()> {
        if self.state != LinkState::Established {
            return Err(Iap2Error::Protocol("Link not established".to_string()));
        }

        debug!(
            "Preparing to send data with seq=0x{:02X}, ack=0x{:02X}",
            self.seq, self.ack
        );
        let packet = Iap2Packet::data(self.seq, self.ack, session_id, payload);

        self.write_packet(stream, &packet).await?;

        loop {
            let ack = self.read_packet_with_timeout(stream).await?;

            match ack.control.packet_type {
                PacketType::Ack => {
                    let has_payload = !ack.payload.is_empty();
                    if has_payload {
                        if !self.pending.iter().any(|p| p.seq == ack.seq) {
                            self.pending.push_back(ack.clone());
                            debug!(
                                "Queued pending control message from ACK packet (seq=0x{:02X}, queue size: {})",
                                ack.seq,
                                self.pending.len()
                            );
                        } else {
                            debug!(
                                "Ignoring duplicate control message in ACK (seq=0x{:02X} already queued)",
                                ack.seq
                            );
                        }
                    }
                    self.ack = ack.seq;

                    if ack.ack == self.seq {
                        debug!("Received ACK for our message (seq=0x{:02X})", self.seq);

                        self.seq = self.seq.wrapping_add(1);
                        debug!("Incremented sequence to 0x{:02X} for next packet", self.seq);

                        break;
                    } else {
                        if has_payload {
                            let response_ack = Iap2Packet::ack(self.seq, self.ack);
                            self.write_packet(stream, &response_ack).await?;
                            debug!(
                                "Sent ACK for iPhone data while waiting: seq=0x{:02X}, ack=0x{:02X}",
                                self.seq, self.ack
                            );
                        }
                        debug!(
                            "Still waiting for ACK: expected 0x{:02X}, got 0x{:02X}",
                            self.seq, ack.ack
                        );
                    }
                }
                PacketType::Eak => {
                    debug!("Received EAK with payload: {}", hex::encode(&ack.payload));

                    if !ack.payload.is_empty() {
                        let highest_received = ack.payload[0];
                        let in_order_ack = ack.ack;

                        if highest_received == self.seq && in_order_ack < self.seq - 1 {
                            let missing_seq = in_order_ack + 1;
                            warn!(
                                "iPhone is missing packet 0x{:02X}! Retransmitting",
                                missing_seq
                            );

                            let retransmit = Iap2Packet::ack(missing_seq, ack.seq);
                            self.write_packet(stream, &retransmit).await?;
                            info!("Retransmitted packet 0x{:02X}", missing_seq);
                        }
                    }

                    self.ack = ack.seq;

                    self.seq = self.next_seq();
                    let eak_ack = Iap2Packet::ack(self.seq, self.ack);
                    self.write_packet(stream, &eak_ack).await?;
                    debug!(
                        "Sent ACK for EAK: seq=0x{:02X}, ack=0x{:02X}",
                        self.seq, self.ack
                    );
                }
                _ => {
                    return Err(Iap2Error::Protocol(format!(
                        "Expected ACK or EAK, got {:?}",
                        ack.control.packet_type
                    )));
                }
            }
        }

        Ok(())
    }

    async fn write_packet(&self, stream: &mut Stream, packet: &Iap2Packet) -> Result<()> {
        let bytes = packet.to_bytes();

        debug!("Sending raw packet: {}", hex::encode(&bytes));
        debug!("Sent packet: {:?}", packet);

        stream.write_all(&bytes).await?;
        stream.flush().await?;

        Ok(())
    }

    async fn read_packet_with_timeout(&mut self, stream: &mut Stream) -> Result<Iap2Packet> {
        match timeout(
            Duration::from_millis(self.config.timeout_ms),
            self.read_packet(stream),
        )
        .await
        {
            Ok(result) => result,
            Err(_) => Err(Iap2Error::Timeout),
        }
    }

    /// Read available bytes from stream into our persistent buffer.
    /// Cancellation-safe: if dropped mid-await, bytes already in read_buf are preserved.
    /// Only the in-flight kernel read is lost, which hasn't been consumed yet.
    async fn fill_buf(&mut self, stream: &mut Stream) -> Result<()> {
        let mut tmp = [0u8; 4096];
        let n = stream.read(&mut tmp).await?;
        if n == 0 {
            return Err(Iap2Error::ConnectionClosed);
        }
        self.read_buf.extend_from_slice(&tmp[..n]);
        Ok(())
    }

    /// Try to parse a complete iAP2 packet from the read buffer.
    /// Returns None if not enough data is available yet.
    /// On parse errors, logs and resyncs instead of propagating.
    fn try_parse_packet(&mut self) -> Result<Option<Iap2Packet>> {
        loop {
            if self.read_buf.len() < 2 {
                return Ok(None);
            }

            if self.read_buf[0] != 0xFF || self.read_buf[1] != 0x5A {
                self.read_buf.advance(1);
                continue;
            }

            if self.read_buf.len() < 4 {
                return Ok(None);
            }

            let packet_len = u16::from_be_bytes([self.read_buf[2], self.read_buf[3]]) as usize;
            if !(6..=8192).contains(&packet_len) {
                warn!("Invalid packet length {} at sync position, resyncing", packet_len);
                self.read_buf.advance(2);
                continue;
            }

            if self.read_buf.len() < packet_len {
                return Ok(None);
            }

            let packet_data = self.read_buf.split_to(packet_len);
            debug!("Received raw packet: {}", hex::encode(&packet_data));

            match Iap2Packet::from_bytes(&packet_data) {
                Ok(packet) => {
                    debug!("Parsed packet: {:?}", packet);
                    return Ok(Some(packet));
                }
                Err(e) => {
                    warn!("Invalid packet ({}), skipping and resyncing", e);
                    continue;
                }
            }
        }
    }

    /// Cancellation-safe packet reader. Uses a persistent buffer so that bytes
    /// read from the stream are never lost if the future is dropped by tokio::select!.
    async fn read_packet(&mut self, stream: &mut Stream) -> Result<Iap2Packet> {
        loop {
            if let Some(packet) = self.try_parse_packet()? {
                return Ok(packet);
            }
            self.fill_buf(stream).await?;
        }
    }

    fn next_seq(&self) -> u8 {
        self.seq + 1
    }

    fn is_seq_after(&self, seq_a: u8, seq_b: u8) -> bool {
        let diff = seq_a.wrapping_sub(seq_b);
        diff > 0 && diff < 128
    }
}

impl Default for Iap2Link {
    fn default() -> Self {
        Self::new(LinkConfig::default())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_try_parse_packet_empty_buffer() {
        let mut link = Iap2Link::default();
        assert!(link.try_parse_packet().unwrap().is_none());
    }

    #[test]
    fn test_try_parse_packet_skips_non_sync() {
        let mut link = Iap2Link::default();
        link.read_buf.extend_from_slice(&[0x00, 0x01, 0x02]);
        assert!(link.try_parse_packet().unwrap().is_none());
        assert!(link.read_buf.is_empty() || link.read_buf.len() < 2);
    }
}
