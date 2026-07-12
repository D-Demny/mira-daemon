use crate::error::{Iap2Error, Result};
use crate::link::Iap2Link;
use crate::types::FileTransferConfig;
use bytes::{BufMut, Bytes, BytesMut};
use std::collections::HashMap;
use std::time::{Duration, Instant};
use tracing::{debug, info, warn};

const DATAGRAM_SETUP: u8 = 0x04;
const DATAGRAM_START: u8 = 0x01;
const DATAGRAM_FIRST_DATA: u8 = 0x80;
const DATAGRAM_DATA: u8 = 0x00;
const DATAGRAM_LAST_DATA: u8 = 0x40;
const DATAGRAM_FIRST_AND_ONLY: u8 = 0xC0;
const DATAGRAM_CANCEL: u8 = 0x02;
const DATAGRAM_PAUSE: u8 = 0x03;
const DATAGRAM_SUCCESS: u8 = 0x05;
const DATAGRAM_FAILURE: u8 = 0x06;

pub const FILE_TYPE_NOW_PLAYING_ARTWORK: u16 = 0x0002;

#[derive(Debug, Clone, PartialEq)]
enum TransferState {
    Setup,
    Receiving,
    Complete,
    #[allow(unused)]
    Failed,
}

#[derive(Debug, Clone)]
struct ActiveTransfer {
    #[allow(unused)]
    transfer_id: u8,
    generation: u64,
    file_type: u16,
    file_size: u64,
    accumulated_data: BytesMut,
    state: TransferState,
    bytes_received: u64,
    setup_timestamp: Instant,
}

#[derive(Debug)]
pub enum FileTransferOutcome {
    Pending,
    Completed {
        transfer_id: u8,
        file_type: u16,
        data: Bytes,
    },
    Corrupted {
        transfer_id: u8,
        reason: String,
    },
}

impl ActiveTransfer {
    fn new(transfer_id: u8, generation: u64, file_type: u16, file_size: u64) -> Self {
        ActiveTransfer {
            transfer_id,
            generation,
            file_type,
            file_size,
            accumulated_data: BytesMut::with_capacity(file_size as usize),
            state: TransferState::Setup,
            bytes_received: 0,
            setup_timestamp: Instant::now(),
        }
    }
}

pub struct FileTransferHandler {
    active_transfers: HashMap<u8, ActiveTransfer>,
    generation_counter: u64,
    recently_cancelled: HashMap<u8, (u64, Instant)>,
    config: FileTransferConfig,
}

impl FileTransferHandler {
    pub fn new(config: FileTransferConfig) -> Self {
        FileTransferHandler {
            active_transfers: HashMap::new(),
            generation_counter: 0,
            recently_cancelled: HashMap::new(),
            config,
        }
    }

    pub async fn handle_datagram(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        payload: &[u8],
    ) -> Result<FileTransferOutcome> {
        if payload.len() < 2 {
            warn!("File transfer datagram too short: {} bytes", payload.len());
            return Ok(FileTransferOutcome::Pending);
        }

        let transfer_id = payload[0];
        let command = payload[1];

        debug!(
            "File transfer datagram: transfer_id=0x{:02X}, command=0x{:02X}, len={}",
            transfer_id,
            command,
            payload.len()
        );

        match command {
            DATAGRAM_SETUP => {
                self.handle_setup(link, stream, file_transfer_session_id, payload)
                    .await?;
                Ok(FileTransferOutcome::Pending)
            }
            DATAGRAM_FIRST_DATA => {
                self.handle_first_data(
                    link,
                    stream,
                    file_transfer_session_id,
                    transfer_id,
                    &payload[2..],
                )
                .await
            }
            DATAGRAM_DATA => {
                self.handle_data(
                    link,
                    stream,
                    file_transfer_session_id,
                    transfer_id,
                    &payload[2..],
                )
                .await
            }
            DATAGRAM_LAST_DATA => {
                self.handle_last_data(
                    link,
                    stream,
                    file_transfer_session_id,
                    transfer_id,
                    &payload[2..],
                )
                .await
            }
            DATAGRAM_FIRST_AND_ONLY => {
                self.handle_first_and_only(
                    link,
                    stream,
                    file_transfer_session_id,
                    transfer_id,
                    &payload[2..],
                )
                .await
            }
            DATAGRAM_CANCEL => {
                info!("File transfer 0x{:02X} cancelled by device", transfer_id);
                if let Some(t) = self.active_transfers.remove(&transfer_id) {
                    self.recently_cancelled
                        .insert(transfer_id, (t.generation, Instant::now()));
                }
                Ok(FileTransferOutcome::Pending)
            }
            DATAGRAM_PAUSE => {
                debug!("File transfer 0x{:02X} paused by device", transfer_id);
                Ok(FileTransferOutcome::Pending)
            }
            _ => {
                warn!("Unknown file transfer command: 0x{:02X}", command);
                Ok(FileTransferOutcome::Pending)
            }
        }
    }

    async fn handle_setup(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        payload: &[u8],
    ) -> Result<()> {
        if payload.len() < 12 {
            return Err(Iap2Error::Protocol(format!(
                "Setup datagram too short: {} bytes",
                payload.len()
            )));
        }

        let transfer_id = payload[0];
        let file_size = u64::from_be_bytes([
            payload[2], payload[3], payload[4], payload[5], payload[6], payload[7], payload[8],
            payload[9],
        ]);
        let file_type = u16::from_be_bytes([payload[10], payload[11]]);

        info!(
            "File transfer setup: id=0x{:02X}, type=0x{:04X}, size={} bytes",
            transfer_id, file_type, file_size
        );

        if file_type != FILE_TYPE_NOW_PLAYING_ARTWORK {
            warn!(
                "Unsupported file type 0x{:04X}, expected NowPlayingArtwork (0x{:04X})",
                file_type, FILE_TYPE_NOW_PLAYING_ARTWORK
            );
        }

        let old_gen = if let Some(old_transfer) = self.active_transfers.remove(&transfer_id) {
            warn!(
                "Setup datagram received for existing transfer 0x{:02X}, resetting state (old gen={})",
                transfer_id, old_transfer.generation
            );
            Some(old_transfer.generation)
        } else {
            self.recently_cancelled.get(&transfer_id).map(|(g, _)| *g)
        };

        if let Some(gen) = old_gen {
            self.recently_cancelled
                .insert(transfer_id, (gen, Instant::now()));
        }

        self.generation_counter += 1;
        let transfer =
            ActiveTransfer::new(transfer_id, self.generation_counter, file_type, file_size);
        self.active_transfers.insert(transfer_id, transfer);

        self.send_start(link, stream, file_transfer_session_id, transfer_id)
            .await?;

        Ok(())
    }

    async fn send_start(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
    ) -> Result<()> {
        let mut datagram = BytesMut::new();
        datagram.put_u8(transfer_id);
        datagram.put_u8(DATAGRAM_START);

        debug!("Sending Start datagram for transfer 0x{:02X}", transfer_id);
        link.send_data(stream, file_transfer_session_id, datagram.freeze())
            .await?;

        Ok(())
    }

    async fn handle_first_data(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
        data: &[u8],
    ) -> Result<FileTransferOutcome> {
        let (state, bytes_received, file_size) = match self.active_transfers.get(&transfer_id) {
            Some(t) => (t.state.clone(), t.bytes_received, t.file_size),
            None => {
                if let Some((_, cancel_time)) = self.recently_cancelled.get(&transfer_id) {
                    if cancel_time.elapsed()
                        < Duration::from_millis(self.config.stale_packet_grace_ms)
                    {
                        debug!(
                            "Ignoring stale FirstData for recently ended transfer 0x{:02X}",
                            transfer_id
                        );
                        return Ok(FileTransferOutcome::Pending);
                    }
                }
                warn!(
                    "FirstData for unknown transfer 0x{:02X}, ignoring",
                    transfer_id
                );
                return Ok(FileTransferOutcome::Pending);
            }
        };

        if state != TransferState::Setup {
            warn!(
                "Received FirstData for 0x{:02X} in invalid state {:?}, expected Setup",
                transfer_id, state
            );
            if let Some(t) = self.active_transfers.remove(&transfer_id) {
                self.recently_cancelled
                    .insert(transfer_id, (t.generation, Instant::now()));
            }
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!("Invalid state for FirstData: {:?}", state),
            });
        }

        let new_total = bytes_received + data.len() as u64;
        if file_size > 0 && new_total > file_size {
            warn!(
                "Transfer 0x{:02X}: FirstData would exceed size ({} > {})",
                transfer_id, new_total, file_size
            );
            if let Some(t) = self.active_transfers.remove(&transfer_id) {
                self.recently_cancelled
                    .insert(transfer_id, (t.generation, Instant::now()));
            }
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!(
                    "Transfer exceeded declared size: {} > {}",
                    new_total, file_size
                ),
            });
        }

        debug!(
            "Received FirstData for 0x{:02X}: {} bytes ({}/{} bytes)",
            transfer_id,
            data.len(),
            new_total,
            file_size
        );

        if let Some(transfer) = self.active_transfers.get_mut(&transfer_id) {
            transfer.accumulated_data.extend_from_slice(data);
            transfer.bytes_received = new_total;
            transfer.state = TransferState::Receiving;
        }

        Ok(FileTransferOutcome::Pending)
    }

    async fn handle_data(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
        data: &[u8],
    ) -> Result<FileTransferOutcome> {
        let (state, bytes_received, file_size) =
            match self.active_transfers.get(&transfer_id) {
                Some(t) => (
                    t.state.clone(),
                    t.bytes_received,
                    t.file_size,
                ),
                None => {
                    if let Some((_, cancel_time)) = self.recently_cancelled.get(&transfer_id) {
                        if cancel_time.elapsed()
                            < Duration::from_millis(self.config.stale_packet_grace_ms)
                        {
                            debug!(
                                "Ignoring stale data for recently ended transfer 0x{:02X}",
                                transfer_id
                            );
                            return Ok(FileTransferOutcome::Pending);
                        }
                    }
                    warn!("Data for unknown transfer 0x{:02X}, ignoring", transfer_id);
                    return Ok(FileTransferOutcome::Pending);
                }
            };

        if state != TransferState::Receiving {
            warn!(
                "Received Data for 0x{:02X} in invalid state {:?}, expected Receiving",
                transfer_id, state
            );
            if let Some(t) = self.active_transfers.remove(&transfer_id) {
                self.recently_cancelled
                    .insert(transfer_id, (t.generation, Instant::now()));
            }
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!("Invalid state for Data: {:?}", state),
            });
        }

        let new_total = bytes_received + data.len() as u64;
        if file_size > 0 && new_total > file_size {
            warn!(
                "Transfer 0x{:02X}: Data would exceed size ({} > {})",
                transfer_id, new_total, file_size
            );
            if let Some(t) = self.active_transfers.remove(&transfer_id) {
                self.recently_cancelled
                    .insert(transfer_id, (t.generation, Instant::now()));
            }
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!(
                    "Transfer exceeded declared size: {} > {}",
                    new_total, file_size
                ),
            });
        }

        debug!(
            "Received Data for 0x{:02X}: {} bytes ({}/{} bytes)",
            transfer_id,
            data.len(),
            new_total,
            file_size
        );

        if let Some(transfer) = self.active_transfers.get_mut(&transfer_id) {
            transfer.accumulated_data.extend_from_slice(data);
            transfer.bytes_received = new_total;
        }

        Ok(FileTransferOutcome::Pending)
    }

    async fn handle_last_data(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
        data: &[u8],
    ) -> Result<FileTransferOutcome> {
        let mut transfer = match self.active_transfers.remove(&transfer_id) {
            Some(t) => t,
            None => {
                if let Some((_, cancel_time)) = self.recently_cancelled.get(&transfer_id) {
                    if cancel_time.elapsed()
                        < Duration::from_millis(self.config.stale_packet_grace_ms)
                    {
                        debug!(
                            "Ignoring stale LastData for recently ended transfer 0x{:02X}",
                            transfer_id
                        );
                        return Ok(FileTransferOutcome::Pending);
                    }
                }
                warn!(
                    "LastData for unknown transfer 0x{:02X}, ignoring",
                    transfer_id
                );
                return Ok(FileTransferOutcome::Pending);
            }
        };

        if transfer.state != TransferState::Receiving {
            warn!(
                "Received LastData for 0x{:02X} in invalid state {:?}, expected Receiving",
                transfer_id, transfer.state
            );
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!("Invalid state for LastData: {:?}", transfer.state),
            });
        }

        let new_total = transfer.bytes_received + data.len() as u64;
        if transfer.file_size > 0 && new_total > transfer.file_size {
            warn!(
                "Transfer 0x{:02X}: LastData would exceed size ({} > {})",
                transfer_id, new_total, transfer.file_size
            );
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!(
                    "Transfer exceeded declared size: {} > {}",
                    new_total, transfer.file_size
                ),
            });
        }

        transfer.accumulated_data.extend_from_slice(data);
        transfer.bytes_received = new_total;
        transfer.state = TransferState::Complete;

        let total_size = transfer.accumulated_data.len();
        let file_type = transfer.file_type;

        info!(
            "File transfer 0x{:02X} complete: {} bytes (expected {})",
            transfer_id, total_size, transfer.file_size
        );

        if transfer.file_size > 0 && total_size as u64 != transfer.file_size {
            let reason = format!(
                "Size mismatch: received {} bytes, expected {}",
                total_size, transfer.file_size
            );
            warn!("Transfer 0x{:02X}: {}", transfer_id, reason);
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason,
            });
        }

        if let Err(reason) = Self::sanitize_artwork(
            &mut transfer.accumulated_data,
            transfer.file_size,
            transfer_id,
        ) {
            warn!(
                "Transfer 0x{:02X}: corrupt artwork detected - {}",
                transfer_id, reason
            );
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason,
            });
        }

        self.send_success(link, stream, file_transfer_session_id, transfer_id)
            .await?;

        self.recently_cancelled
            .insert(transfer_id, (transfer.generation, Instant::now()));

        let artwork = transfer.accumulated_data.freeze();

        Ok(FileTransferOutcome::Completed {
            transfer_id,
            file_type,
            data: artwork,
        })
    }

    async fn handle_first_and_only(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
        data: &[u8],
    ) -> Result<FileTransferOutcome> {
        let mut transfer = match self.active_transfers.remove(&transfer_id) {
            Some(t) => t,
            None => {
                if let Some((_, cancel_time)) = self.recently_cancelled.get(&transfer_id) {
                    if cancel_time.elapsed()
                        < Duration::from_millis(self.config.stale_packet_grace_ms)
                    {
                        debug!(
                            "Ignoring stale FirstAndOnlyData for recently ended transfer 0x{:02X}",
                            transfer_id
                        );
                        return Ok(FileTransferOutcome::Pending);
                    }
                }
                warn!(
                    "FirstAndOnlyData for unknown transfer 0x{:02X}, ignoring",
                    transfer_id
                );
                return Ok(FileTransferOutcome::Pending);
            }
        };

        if transfer.file_size > 0 && data.len() as u64 > transfer.file_size {
            warn!(
                "Transfer 0x{:02X}: FirstAndOnlyData exceeds declared size ({} > {})",
                transfer_id,
                data.len(),
                transfer.file_size
            );
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason: format!(
                    "Transfer exceeded declared size: {} > {}",
                    data.len(),
                    transfer.file_size
                ),
            });
        }

        debug!(
            "Received FirstAndOnlyData for 0x{:02X}: {} bytes (expected {})",
            transfer_id,
            data.len(),
            transfer.file_size
        );

        transfer.accumulated_data.extend_from_slice(data);
        transfer.bytes_received = data.len() as u64;
        transfer.state = TransferState::Complete;

        let total_size = transfer.accumulated_data.len();
        let file_type = transfer.file_type;

        info!(
            "File transfer 0x{:02X} complete (single packet): {} bytes (expected {})",
            transfer_id, total_size, transfer.file_size
        );

        if transfer.file_size > 0 && total_size as u64 != transfer.file_size {
            let reason = format!(
                "Size mismatch: received {} bytes, expected {}",
                total_size, transfer.file_size
            );
            warn!("Transfer 0x{:02X}: {}", transfer_id, reason);
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason,
            });
        }

        if let Err(reason) = Self::sanitize_artwork(
            &mut transfer.accumulated_data,
            transfer.file_size,
            transfer_id,
        ) {
            warn!(
                "Transfer 0x{:02X}: corrupt artwork detected - {}",
                transfer_id, reason
            );
            self.recently_cancelled
                .insert(transfer_id, (transfer.generation, Instant::now()));
            self.send_failure(link, stream, file_transfer_session_id, transfer_id)
                .await?;
            return Ok(FileTransferOutcome::Corrupted {
                transfer_id,
                reason,
            });
        }

        self.send_success(link, stream, file_transfer_session_id, transfer_id)
            .await?;

        self.recently_cancelled
            .insert(transfer_id, (transfer.generation, Instant::now()));

        let artwork = transfer.accumulated_data.freeze();

        Ok(FileTransferOutcome::Completed {
            transfer_id,
            file_type,
            data: artwork,
        })
    }

    async fn send_success(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
    ) -> Result<()> {
        let mut datagram = BytesMut::new();
        datagram.put_u8(transfer_id);
        datagram.put_u8(DATAGRAM_SUCCESS);

        debug!(
            "Sending Success datagram for transfer 0x{:02X}",
            transfer_id
        );
        link.send_data(stream, file_transfer_session_id, datagram.freeze())
            .await?;

        Ok(())
    }

    async fn send_failure(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        file_transfer_session_id: u8,
        transfer_id: u8,
    ) -> Result<()> {
        let mut datagram = BytesMut::new();
        datagram.put_u8(transfer_id);
        datagram.put_u8(DATAGRAM_FAILURE);

        debug!(
            "Sending Failure datagram for transfer 0x{:02X}",
            transfer_id
        );
        link.send_data(stream, file_transfer_session_id, datagram.freeze())
            .await?;

        Ok(())
    }

    fn sanitize_artwork(
        data: &mut BytesMut,
        expected_size: u64,
        transfer_id: u8,
    ) -> std::result::Result<(), String> {
        if data.is_empty() {
            if expected_size == 0 {
                debug!(
                    "Transfer 0x{:02X}: received empty artwork payload as expected",
                    transfer_id
                );
                return Ok(());
            } else {
                return Err("artwork payload is empty".to_string());
            }
        }

        let end_pos = Self::validate_jpeg_structure(data)?;

        if end_pos < data.len() {
            let trailing = data.len() - end_pos;
            let trailing_data = &data[end_pos..];
            for i in 0..trailing_data.len().saturating_sub(1) {
                if trailing_data[i] == 0xFF && trailing_data[i + 1] == 0xD8 {
                    return Err(format!(
                        "corrupted: found additional JPEG SOI marker {} bytes after EOI (total trailing: {})",
                        i, trailing
                    ));
                }
            }
            warn!(
                "Transfer 0x{:02X}: trimming {} trailing byte(s) after JPEG EOI",
                transfer_id, trailing
            );
            data.truncate(end_pos);
        }

        Ok(())
    }

    fn validate_jpeg_structure(data: &[u8]) -> std::result::Result<usize, String> {
        if data.len() < 4 {
            return Err("JPEG data too short".to_string());
        }

        if data[0] != 0xFF || data[1] != 0xD8 {
            return Err("missing JPEG SOI marker".to_string());
        }

        let mut pos = 2;
        while pos < data.len() {
            if data[pos] != 0xFF {
                pos += 1;
                continue;
            }

            while pos < data.len() && data[pos] == 0xFF {
                pos += 1;
            }
            if pos >= data.len() {
                return Err("truncated JPEG marker".to_string());
            }

            let marker = data[pos];
            pos += 1;

            if marker == 0xD9 {
                return Ok(pos);
            }

            if marker == 0x00 || marker == 0x01 || (0xD0..=0xD7).contains(&marker) {
                continue;
            }

            if marker == 0xDA {
                if pos + 2 > data.len() {
                    return Err("truncated SOS header".to_string());
                }
                let sos_len = u16::from_be_bytes([data[pos], data[pos + 1]]) as usize;
                if sos_len < 2 || pos + sos_len > data.len() {
                    return Err(format!("invalid SOS length: {}", sos_len));
                }
                pos += sos_len;

                while pos < data.len() - 1 {
                    if data[pos] == 0xFF {
                        let next = data[pos + 1];
                        if next == 0xD9 {
                            return Ok(pos + 2);
                        }
                        if next == 0x00 || (0xD0..=0xD7).contains(&next) {
                            pos += 2;
                            continue;
                        }
                    }
                    pos += 1;
                }
                return Err("missing EOI after scan data".to_string());
            }

            if pos + 2 > data.len() {
                return Err(format!(
                    "truncated segment header for marker 0x{:02X}",
                    marker
                ));
            }
            let seg_len = u16::from_be_bytes([data[pos], data[pos + 1]]) as usize;
            if seg_len < 2 {
                return Err(format!(
                    "invalid segment length {} for marker 0x{:02X}",
                    seg_len, marker
                ));
            }
            if pos + seg_len > data.len() {
                return Err(format!(
                    "truncated segment for marker 0x{:02X}: need {} bytes, have {}",
                    marker,
                    seg_len,
                    data.len() - pos
                ));
            }
            pos += seg_len;
        }

        Err("missing JPEG EOI marker".to_string())
    }

    /// Clean up stale transfers and return IDs of timed-out transfers
    /// so the caller can emit FileTransferCorrupted events and notify the UI.
    pub fn cleanup_stale(&mut self) -> Vec<u8> {
        let now = Instant::now();
        let transfer_timeout = Duration::from_secs(self.config.transfer_timeout_secs);
        let stale_grace = Duration::from_millis(self.config.stale_packet_grace_ms);

        let mut timed_out = Vec::new();

        self.active_transfers.retain(|id, transfer| {
            let elapsed = now.duration_since(transfer.setup_timestamp);
            if elapsed >= transfer_timeout {
                warn!(
                    "Transfer 0x{:02X} timed out after {:?} (received {}/{} bytes), removing",
                    id, elapsed, transfer.bytes_received, transfer.file_size
                );
                timed_out.push(*id);
                false
            } else {
                true
            }
        });

        for &id in &timed_out {
            self.generation_counter += 1;
            self.recently_cancelled
                .insert(id, (self.generation_counter, Instant::now()));
        }

        self.recently_cancelled
            .retain(|_, (_, cancel_time)| cancel_time.elapsed() < stale_grace * 2);

        timed_out
    }
}

impl Default for FileTransferHandler {
    fn default() -> Self {
        Self::new(FileTransferConfig::default())
    }
}
