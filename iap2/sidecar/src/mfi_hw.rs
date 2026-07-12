// Hardware MFi auth provider for the onboard Apple CP 3.0 coprocessor via the stock kernel proprietary apple_mfi_auth driver

use std::fs::{File, OpenOptions};
use std::os::unix::io::AsRawFd;

use async_trait::async_trait;
use iap2_rs::{Iap2Error, MfiAuthProvider, Result as Iap2Result};
use tracing::info;

const MFI_DEVICE_PATH: &str = "/dev/apple_mfi";

// _IOR/_IOW magic 0x77, 16-byte param struct {u32 size, u32 pad, u64 buf_ptr}
const MFI_IOCTL_GET_CERT_LEN: u32 = 0x80107704;
const MFI_IOCTL_GET_CERT: u32 = 0x80107705;
const MFI_IOCTL_SET_CHALLENGE: u32 = 0x40107706;
const MFI_IOCTL_GET_RESPONSE: u32 = 0x80107707;

const MFI_CHALLENGE_SIZE: usize = 32;
const MFI_RESPONSE_SIZE: usize = 64;

#[repr(C)]
struct MfiIoctlParam {
    size: u32,
    pad: u32,
    buf_ptr: u64,
}

fn open_dev() -> std::io::Result<File> {
    OpenOptions::new()
        .read(true)
        .write(true)
        .open(MFI_DEVICE_PATH)
}

fn ioctl_xfer(file: &File, req: u32, size: u32, buf: &mut [u8]) -> std::io::Result<()> {
    let param = MfiIoctlParam {
        size,
        pad: 0,
        buf_ptr: buf.as_mut_ptr() as u64,
    };
    let rc = unsafe { libc::ioctl(file.as_raw_fd(), req as _, &param as *const MfiIoctlParam) };
    if rc < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

pub fn read_certificate_blocking() -> Result<Vec<u8>, String> {
    let file = open_dev().map_err(|e| format!("open {}: {}", MFI_DEVICE_PATH, e))?;
    let mut len_buf = vec![0u8; 3];
    ioctl_xfer(&file, MFI_IOCTL_GET_CERT_LEN, 2, &mut len_buf)
        .map_err(|e| format!("ioctl GET_CERT_LEN: {}", e))?;
    let cert_len = u16::from_be_bytes([len_buf[0], len_buf[1]]) as usize;
    if cert_len == 0 || cert_len > 1024 {
        return Err(format!("implausible certificate length {}", cert_len));
    }

    let file = open_dev().map_err(|e| format!("reopen {}: {}", MFI_DEVICE_PATH, e))?;
    let mut cert_buf = vec![0u8; cert_len + 1];
    ioctl_xfer(&file, MFI_IOCTL_GET_CERT, cert_len as u32, &mut cert_buf)
        .map_err(|e| format!("ioctl GET_CERT: {}", e))?;
    cert_buf.truncate(cert_len);

    if cert_buf.iter().all(|&b| b == 0x00) || cert_buf.iter().all(|&b| b == 0xFF) {
        return Err("certificate is all-zeros/all-FF, chip read failed".into());
    }
    info!("MFi certificate read: {} bytes", cert_len);
    Ok(cert_buf)
}

pub fn challenge_response_blocking(challenge: &[u8]) -> Result<Vec<u8>, String> {
    if challenge.len() != MFI_CHALLENGE_SIZE {
        return Err(format!("challenge must be 32 bytes, got {}", challenge.len()));
    }
    let file = open_dev().map_err(|e| format!("open {}: {}", MFI_DEVICE_PATH, e))?;

    let mut chal = [0u8; MFI_CHALLENGE_SIZE];
    chal.copy_from_slice(challenge);
    ioctl_xfer(&file, MFI_IOCTL_SET_CHALLENGE, 32, &mut chal)
        .map_err(|e| format!("ioctl SET_CHALLENGE: {}", e))?;

    // chip signs asynchronously
    std::thread::sleep(std::time::Duration::from_millis(100));

    let mut resp = vec![0u8; MFI_RESPONSE_SIZE];
    ioctl_xfer(&file, MFI_IOCTL_GET_RESPONSE, 64, &mut resp)
        .map_err(|e| format!("ioctl GET_RESPONSE: {}", e))?;

    if resp.iter().all(|&b| b == 0x00) || resp.iter().all(|&b| b == 0xFF) {
        return Err("all-zero/all-FF signature, hardware error".into());
    }
    if resp.iter().filter(|&&b| (0x20..=0x7E).contains(&b)).count() > 32 {
        return Err(format!(
            "response looks like ASCII (serial number?), not a signature: {}",
            String::from_utf8_lossy(&resp)
        ));
    }
    if resp[0..32].iter().all(|&b| b == 0) || resp[32..64].iter().all(|&b| b == 0) {
        return Err("ECDSA r or s component is zero, invalid signature".into());
    }
    info!("MFi chip returned 64-byte ECDSA signature");
    Ok(resp)
}

pub struct HardwareMfiProvider;

#[async_trait]
impl MfiAuthProvider for HardwareMfiProvider {
    async fn read_certificate(&self) -> Iap2Result<Vec<u8>> {
        tokio::task::spawn_blocking(read_certificate_blocking)
            .await
            .map_err(|e| Iap2Error::Mfi(e.to_string()))?
            .map_err(Iap2Error::Mfi)
    }

    async fn challenge_response(&self, challenge: &[u8]) -> Iap2Result<Vec<u8>> {
        let chal = challenge.to_vec();
        tokio::task::spawn_blocking(move || challenge_response_blocking(&chal))
            .await
            .map_err(|e| Iap2Error::Mfi(e.to_string()))?
            .map_err(Iap2Error::Mfi)
    }
}
