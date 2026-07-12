// iAP2 sidecar for mira-daemon

mod mfi_hw;
mod session;

use serde::Deserialize;
use tokio::io::{AsyncBufReadExt, BufReader};
use tracing::{info, warn};

#[derive(Deserialize)]
struct Command {
    cmd: String,
    addr: Option<String>,
    channel: Option<u8>,
    steps: Option<i32>,
}

pub fn emit(event: &serde_json::Value) {
    // one JSON object per line
    println!("{}", event);
}

fn emit_error(err: &str) {
    emit(&serde_json::json!({"event": "error", "error": err}));
}

fn ensure_mfi_node() {
    const PATH: &str = "/dev/apple_mfi";
    if std::path::Path::new(PATH).exists() {
        return;
    }
    let cpath = std::ffi::CString::new(PATH).unwrap();
    let dev = libc::makedev(510, 0);
    let rc = unsafe { libc::mknod(cpath.as_ptr(), libc::S_IFCHR | 0o600, dev) };
    if rc != 0 {
        warn!("mknod {}: {}", PATH, std::io::Error::last_os_error());
    } else {
        info!("created {} (c 510 0)", PATH);
    }
}

fn probe() -> Result<(), String> {
    ensure_mfi_node();
    let cert = mfi_hw::read_certificate_blocking()?;
    println!("certificate: {} bytes", cert.len());
    println!("  head: {}", hex::encode(&cert[..cert.len().min(32)]));
    let challenge: Vec<u8> = (1..=32u8).collect();
    let sig = mfi_hw::challenge_response_blocking(&challenge)?;
    println!("signature: {} bytes ({}...)", sig.len(), hex::encode(&sig[..8]));
    println!("PROBE PASS");
    Ok(())
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_max_level(tracing::Level::INFO)
        .init();

    if std::env::args().nth(1).as_deref() == Some("probe") {
        if let Err(e) = probe() {
            eprintln!("FAIL: {}", e);
            std::process::exit(1);
        }
        return;
    }

    ensure_mfi_node();
    let mgr = session::Manager::new();
    emit(&serde_json::json!({"event": "ready"}));

    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    // stdin EOF = daemon went away
    while let Ok(Some(line)) = lines.next_line().await {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let cmd: Command = match serde_json::from_str(line) {
            Ok(c) => c,
            Err(e) => {
                emit_error(&format!("bad command: {}", e));
                continue;
            }
        };
        match cmd.cmd.as_str() {
            "connect" => match cmd.addr {
                Some(addr) => mgr.connect(addr, cmd.channel.unwrap_or(1)).await,
                None => emit_error("connect requires addr"),
            },
            "disconnect" => mgr.disconnect().await,
            "volume" => match cmd.steps {
                Some(steps) if steps != 0 => mgr.volume(steps).await,
                _ => emit_error("volume requires non-zero steps"),
            },
            "ping" => emit(&serde_json::json!({"event": "pong"})),
            other => emit_error(&format!("unknown cmd: {}", other)),
        }
    }
    info!("stdin closed, shutting down");
    mgr.disconnect().await;
}
