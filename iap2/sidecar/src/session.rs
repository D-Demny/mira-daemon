// iAP2 session lifecycle

use std::sync::Arc;

use iap2_rs::{
    connect, ConnectionConfig, DeviceIdentification, FileTransferConfig, HidCommand, HidComponent,
    HidConfig, HidFunction, Iap2Config, Iap2Connection, LinkConfig, NowPlayingConfig, PowerConfig,
};
use tokio::sync::Mutex;
use tracing::{info, warn};

use crate::emit;
use crate::mfi_hw::HardwareMfiProvider;

const CONNECT_ATTEMPTS: u32 = 3;
const CONNECT_RETRY_GAP: std::time::Duration = std::time::Duration::from_secs(6);
const STEP_GAP: std::time::Duration = std::time::Duration::from_millis(80);

fn adapter_mac() -> Result<Vec<u8>, String> {
    // BluetoothTransportComponent must carry the real adapter addres
    let addr = std::fs::read_dir("/var/lib/bluetooth")
        .map_err(|e| format!("read /var/lib/bluetooth: {}", e))?
        .filter_map(|e| e.ok())
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .find(|n| n.len() == 17 && n.matches(':').count() == 5)
        .ok_or("no adapter dir in /var/lib/bluetooth")?;
    addr.split(':')
        .map(|b| u8::from_str_radix(b, 16).map_err(|e| format!("bad mac byte: {}", e)))
        .collect()
}

fn identification() -> Result<DeviceIdentification, String> {
    Ok(DeviceIdentification {
        name: "Mira".to_string(),
        model_identifier: "YX5H6679".to_string(),
        manufacturer: "Mira".to_string(),
        serial_number: "00000001".to_string(),
        firmware_version: "1.0.0".to_string(),
        hardware_version: "1".to_string(),
        messages_sent: vec![0x6800, 0x6802, 0x6803],
        messages_received: vec![0x6801],
        // empty = no iOS "app not installed" prompt
        ea_protocol_name: String::new(),
        bundle_seed_id: None,
        current_language: "en".to_string(),
        supported_languages: vec!["en".to_string()],
        hid_components: vec![
            HidComponent {
                id: 0x1092,
                name: "Mira".to_string(),
                function: HidFunction::None,
                extra_data: Some(adapter_mac()?),
            },
            HidComponent {
                id: 0x14E9,
                name: "Mira".to_string(),
                function: HidFunction::MediaPlaybackRemote,
                extra_data: None,
            },
        ],
    })
}

fn emit_state(state: &str, addr: &str) {
    emit(&serde_json::json!({"event": "state", "state": state, "addr": addr}));
}

struct Active {
    addr: String,
    conn: Iap2Connection,
}

pub struct Manager {
    active: Arc<Mutex<Option<Active>>>,
}

impl Manager {
    pub fn new() -> Self {
        Self {
            active: Arc::new(Mutex::new(None)),
        }
    }

    pub async fn connect(&self, addr: String, channel: u8) {
        let mut guard = self.active.lock().await;
        if let Some(a) = guard.as_ref() {
            if a.addr == addr && a.conn.is_running().await {
                emit_state("connected", &addr);
                return;
            }
        }
        *guard = None;

        emit_state("connecting", &addr);
        match establish(&addr, channel).await {
            Ok(mut conn) => {
                let events =
                    std::mem::replace(&mut conn.events, tokio::sync::mpsc::unbounded_channel().1);
                spawn_event_pump(events, addr.clone(), Arc::clone(&self.active));
                *guard = Some(Active {
                    addr: addr.clone(),
                    conn,
                });
                emit_state("connected", &addr);
            }
            Err(e) => {
                emit(&serde_json::json!({"event": "error", "error": e}));
                emit_state("disconnected", &addr);
            }
        }
    }

    pub async fn disconnect(&self) {
        let mut guard = self.active.lock().await;
        if let Some(a) = guard.take() {
            info!("dropping iAP2 session to {}", a.addr);
            emit_state("disconnected", &a.addr);
        }
    }

    pub async fn volume(&self, steps: i32) {
        let guard = self.active.lock().await;
        let Some(a) = guard.as_ref() else {
            emit(&serde_json::json!({"event": "error", "error": "no session"}));
            return;
        };
        let hid = if steps > 0 {
            HidCommand::VolumeUp
        } else {
            HidCommand::VolumeDown
        };
        for _ in 0..steps.unsigned_abs().min(16) {
            if let Err(e) = a.conn.send_hid_command(hid) {
                emit(&serde_json::json!({"event": "error", "error": format!("hid send: {}", e)}));
                return;
            }
            tokio::time::sleep(STEP_GAP).await;
        }
    }
}

async fn establish(addr: &str, channel: u8) -> Result<Iap2Connection, String> {
    let ba: bluer::Address = addr.parse().map_err(|e| format!("bad addr: {}", e))?;
    let sa = bluer::rfcomm::SocketAddr::new(ba, channel);

    let mut last_err = String::new();
    for attempt in 1..=CONNECT_ATTEMPTS {
        match try_once(sa).await {
            Ok(conn) => return Ok(conn),
            Err(e) => {
                warn!("iAP2 connect attempt {}/{}: {}", attempt, CONNECT_ATTEMPTS, e);
                last_err = e;
                if attempt < CONNECT_ATTEMPTS {
                    tokio::time::sleep(CONNECT_RETRY_GAP).await;
                }
            }
        }
    }
    Err(last_err)
}

async fn try_once(sa: bluer::rfcomm::SocketAddr) -> Result<Iap2Connection, String> {
    let stream = bluer::rfcomm::Stream::connect(sa)
        .await
        .map_err(|e| format!("rfcomm connect: {}", e))?;
    let config = Iap2Config {
        identification: identification()?,
        mfi_provider: Arc::new(HardwareMfiProvider),
        enable_now_playing: false,
        enable_hid: true,
        link_config: LinkConfig::default(),
        now_playing_config: NowPlayingConfig::default(),
        hid_config: HidConfig::default(),
        file_transfer_config: FileTransferConfig::default(),
        connection_config: ConnectionConfig::default(),
        power_config: PowerConfig::default(),
    };
    connect(stream, config)
        .await
        .map_err(|e| format!("iap2 connect: {}", e))
}

// Forward connection events to the daemon
fn spawn_event_pump(
    mut events: tokio::sync::mpsc::UnboundedReceiver<iap2_rs::ConnectionEvent>,
    addr: String,
    active: Arc<Mutex<Option<Active>>>,
) {
    tokio::spawn(async move {
        while let Some(ev) = events.recv().await {
            info!("iAP2 event: {:?}", ev);
            if matches!(ev, iap2_rs::ConnectionEvent::Disconnected) {
                let mut guard = active.lock().await;
                if guard.as_ref().map(|a| a.addr == addr).unwrap_or(false) {
                    *guard = None;
                    emit_state("disconnected", &addr);
                }
                break;
            }
        }
    });
}
