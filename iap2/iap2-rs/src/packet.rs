use bytes::{BufMut, Bytes, BytesMut};
use std::fmt;

use crate::error::{Iap2Error, Result};

const SYNC_BYTE: u8 = 0xFF;
const SOP_BYTE: u8 = 0x5A;
const PACKET_HEADER_SIZE: usize = 9;

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum PacketType {
    Syn = 0x80,
    SynAck = 0xC0,
    Ack = 0x40,
    Data = 0x00,
    Eak = 0x60,
    Rst = 0xA0,
    Suspend = 0xE0,
    SuspendAck = 0xF0,
    Detect = 0xEE,
}

impl TryFrom<u8> for PacketType {
    type Error = Iap2Error;

    fn try_from(value: u8) -> Result<Self> {
        if value == 0xEE {
            return Ok(PacketType::Detect);
        }

        match value & 0xF0 {
            0x80 => Ok(PacketType::Syn),
            0xC0 => Ok(PacketType::SynAck),
            0x40 => Ok(PacketType::Ack),
            0x00 => Ok(PacketType::Data),
            0x60 => Ok(PacketType::Eak),
            0xA0 => Ok(PacketType::Rst),
            0xE0 => Ok(PacketType::Suspend),
            0xF0 => Ok(PacketType::SuspendAck),
            _ => Err(Iap2Error::InvalidPacket(format!(
                "Unknown packet type: 0x{:02X}",
                value
            ))),
        }
    }
}

#[derive(Debug, Clone)]
pub struct ControlByte {
    pub packet_type: PacketType,
    pub has_session: bool,
}

impl ControlByte {
    pub fn new(packet_type: PacketType) -> Self {
        ControlByte {
            packet_type,
            has_session: false,
        }
    }

    pub fn to_byte(&self) -> u8 {
        (self.packet_type as u8) | (if self.has_session { 0x08 } else { 0x00 })
    }

    pub fn from_byte(byte: u8) -> Result<Self> {
        let packet_type = PacketType::try_from(byte)?;
        Ok(ControlByte {
            packet_type,
            has_session: (byte & 0x08) != 0,
        })
    }
}

#[derive(Clone)]
pub struct Iap2Packet {
    pub control: ControlByte,
    pub seq: u8,
    pub ack: u8,
    pub session_id: Option<u8>,
    pub payload: Bytes,
}

impl Iap2Packet {
    pub fn new(control: ControlByte, seq: u8, ack: u8) -> Self {
        Iap2Packet {
            control,
            seq,
            ack,
            session_id: None,
            payload: Bytes::new(),
        }
    }

    pub fn detect() -> Self {
        let control = ControlByte::new(PacketType::Detect);
        Iap2Packet::new(control, 0x10, 0x00)
    }

    pub fn syn(seq: u8) -> Self {
        let control = ControlByte::new(PacketType::Syn);
        let mut packet = Iap2Packet::new(control, seq, 0x00);

        let mut payload = BytesMut::new();
        payload.put_u8(0x01);
        payload.put_u8(0x05);
        payload.put_u8(0x10);
        payload.put_u8(0x00);
        payload.put_u8(0x04);
        payload.put_u8(0x0B);
        payload.put_u8(0x00);
        payload.put_u8(0x17);
        payload.put_u8(0x03);
        payload.put_u8(0x03);
        payload.put_u8(0x01);
        payload.put_u8(0x01);
        payload.put_u8(0x02);
        payload.put_u8(0x0A);
        payload.put_u8(0x00);
        payload.put_u8(0x01);
        payload.put_u8(0x0B);
        payload.put_u8(0x02);
        payload.put_u8(0x01);

        packet.payload = payload.freeze();
        packet
    }

    pub fn ack(seq: u8, ack: u8) -> Self {
        let control = ControlByte::new(PacketType::Ack);
        Iap2Packet::new(control, seq, ack)
    }

    pub fn ack_with_session(seq: u8, ack: u8, session_id: u8) -> Self {
        let mut p = Self::ack(seq, ack);
        p.control.has_session = true;
        p.session_id = Some(session_id);
        p
    }

    pub fn data(seq: u8, ack: u8, session_id: u8, payload: Bytes) -> Self {
        let mut control = ControlByte::new(PacketType::Ack);
        control.has_session = true;

        let mut packet = Iap2Packet::new(control, seq, ack);
        packet.session_id = Some(session_id);
        packet.payload = payload;
        packet
    }

    pub fn to_bytes(&self) -> Bytes {
        let mut buf = BytesMut::with_capacity(PACKET_HEADER_SIZE + self.payload.len());

        buf.put_u8(SYNC_BYTE);
        buf.put_u8(SOP_BYTE);

        if self.control.packet_type == PacketType::Detect {
            buf.put_u16(0x0006);
            buf.put_u8(self.control.to_byte());
            buf.put_u8(self.seq);
            return buf.freeze();
        }

        let has_payload = !self.payload.is_empty();
        let packet_len = if has_payload {
            PACKET_HEADER_SIZE as u16 + self.payload.len() as u16 + 1
        } else {
            PACKET_HEADER_SIZE as u16 + self.payload.len() as u16
        };

        buf.put_u16(packet_len);

        buf.put_u8(self.control.to_byte());
        buf.put_u8(self.seq);
        buf.put_u8(self.ack);

        if let Some(session) = self.session_id {
            buf.put_u8(session);
        } else {
            buf.put_u8(0x00);
        }

        #[cfg(debug_assertions)]
        {
            let mut sum: i32 = 0;
            for &b in &buf[0..8] {
                sum = (sum + (b as i8 as i32)) & 0xFF;
            }
            tracing::debug!("Header pre-sum (signed 8-bit) = 0x{:02X}", (sum as u8));
        }
        let header_checksum = Self::calculate_header_checksum(&buf[0..8]);
        buf.put_u8(header_checksum);

        if has_payload {
            buf.put_slice(&self.payload);

            let payload_checksum = Self::calculate_payload_checksum(&self.payload);
            buf.put_u8(payload_checksum);
        }

        buf.freeze()
    }

    pub fn from_bytes(data: &[u8]) -> Result<Self> {
        if data.len() < 6 {
            return Err(Iap2Error::InvalidPacket(format!(
                "Packet too short: {} bytes",
                data.len()
            )));
        }

        if data[0] != SYNC_BYTE || data[1] != SOP_BYTE {
            return Err(Iap2Error::InvalidPacket(
                "Invalid sync/SOP bytes".to_string(),
            ));
        }

        let packet_len = u16::from_be_bytes([data[2], data[3]]) as usize;
        if data.len() < packet_len {
            return Err(Iap2Error::InvalidPacket(format!(
                "Incomplete packet: expected {} bytes, got {}",
                packet_len,
                data.len()
            )));
        }

        let control = ControlByte::from_byte(data[4])?;

        if control.packet_type == PacketType::Detect && packet_len == 6 {
            return Ok(Iap2Packet {
                control,
                seq: data[5],
                ack: 0,
                session_id: None,
                payload: Bytes::new(),
            });
        }

        if data.len() < PACKET_HEADER_SIZE {
            return Err(Iap2Error::InvalidPacket(format!(
                "Packet too short for type {:?}: {} bytes",
                control.packet_type,
                data.len()
            )));
        }

        let seq = data[5];
        let ack = data[6];
        let session_id = if data[7] != 0 { Some(data[7]) } else { None };

        #[cfg(debug_assertions)]
        {
            let mut dbg_sum: i32 = 0;
            for &byte in data.iter().take(9) {
                dbg_sum = (dbg_sum + (byte as i8 as i32)) & 0xFF;
            }
            tracing::trace!(
                "Header sum check (signed 8-bit) = 0x{:02X}",
                (dbg_sum as u8)
            );
        }
        let mut sum: u8 = 0;
        for &byte in data.iter().take(9) {
            sum = sum.wrapping_add(byte);
        }
        if sum != 0 {
            return Err(Iap2Error::InvalidPacket(format!(
                "Header checksum verification failed: sum=0x{:02X} (expected 0)",
                sum
            )));
        }

        let payload_start = PACKET_HEADER_SIZE;
        let payload = if packet_len > payload_start {
            let payload_data = &data[payload_start..packet_len];

            if !payload_data.is_empty() {
                let mut payload_sum: u8 = 0;
                for &byte in payload_data {
                    payload_sum = payload_sum.wrapping_add(byte);
                }
                if payload_sum != 0 {
                    return Err(Iap2Error::InvalidPacket(format!(
                        "Payload checksum verification failed: sum=0x{:02X} (expected 0)",
                        payload_sum
                    )));
                }
                Bytes::copy_from_slice(&payload_data[..payload_data.len() - 1])
            } else {
                Bytes::new()
            }
        } else {
            Bytes::new()
        };

        Ok(Iap2Packet {
            control,
            seq,
            ack,
            session_id,
            payload,
        })
    }

    fn calculate_header_checksum(data: &[u8]) -> u8 {
        let mut sum: u8 = 0;
        for &byte in data {
            sum = sum.wrapping_add(byte);
        }
        (!sum).wrapping_add(1)
    }

    fn calculate_payload_checksum(data: &[u8]) -> u8 {
        let mut sum: u8 = 0;
        for &byte in data {
            sum = sum.wrapping_add(byte);
        }
        (!sum).wrapping_add(1)
    }
}

impl fmt::Debug for Iap2Packet {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "Iap2Packet {{ type: {:?}, seq: 0x{:02X}, ack: 0x{:02X}, session: {:?}, payload_len: {} }}",
            self.control.packet_type,
            self.seq,
            self.ack,
            self.session_id,
            self.payload.len()
        )
    }
}
