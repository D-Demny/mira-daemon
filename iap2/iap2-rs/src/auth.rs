use std::sync::Arc;

use bluer::rfcomm::Stream;
use bytes::{BufMut, BytesMut};
use tracing::{debug, error, info};

use crate::error::{Iap2Error, Result};
use crate::link::Iap2Link;
use crate::mfi::MfiAuthProvider;

const IAP2_MSG_ID_AUTH_CERT: u16 = 0xAA01;
const IAP2_MSG_ID_AUTH_RESPONSE: u16 = 0xAA03;

pub struct Iap2Auth {
    certificate: Option<Vec<u8>>,
    mfi_provider: Arc<dyn MfiAuthProvider>,
}

impl Iap2Auth {
    pub fn new(mfi_provider: Arc<dyn MfiAuthProvider>) -> Self {
        Iap2Auth {
            certificate: None,
            mfi_provider,
        }
    }

    async fn load_certificate(&self) -> Result<Vec<u8>> {
        self.mfi_provider
            .read_certificate()
            .await
            .map_err(|e| {
                error!("Failed to read certificate from MFi provider: {}", e);
                e
            })
            .inspect(|cert| {
                info!(
                    "Successfully loaded certificate from MFi provider: {} bytes",
                    cert.len()
                );
            })
    }

    pub async fn handle_certificate_request(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut Stream,
        session_id: u8,
    ) -> Result<()> {
        info!("Handling authentication certificate request");

        if self.certificate.is_none() {
            self.certificate = Some(self.load_certificate().await?);
        }

        self.send_certificate(link, stream, session_id).await
    }

    pub async fn handle_challenge_request(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut Stream,
        session_id: u8,
        payload: &[u8],
    ) -> Result<()> {
        info!("Handling authentication challenge request");

        if payload.len() < 4 {
            return Err(Iap2Error::AuthenticationFailed(
                "Challenge payload too short".to_string(),
            ));
        }

        let param_len = u16::from_be_bytes([payload[0], payload[1]]) as usize;
        if payload.len() < param_len {
            return Err(Iap2Error::AuthenticationFailed(format!(
                "Challenge payload incomplete: expected {}, got {}",
                param_len,
                payload.len()
            )));
        }

        if payload.len() < 4 + 32 {
            return Err(Iap2Error::AuthenticationFailed(format!(
                "Payload too short for 32-byte challenge: got {}",
                payload.len()
            )));
        }

        let full_challenge = &payload[4..4 + 32];
        debug!(
            "Received challenge: {} bytes (param_len={})",
            full_challenge.len(),
            param_len
        );
        debug!("Full payload hex: {}", hex::encode(payload));
        debug!("Full challenge hex: {}", hex::encode(full_challenge));

        if full_challenge.len() != 32 {
            return Err(Iap2Error::AuthenticationFailed(format!(
                "Challenge must be 32 bytes, got {}",
                full_challenge.len()
            )));
        }

        let challenge = &full_challenge[0..32];
        debug!(
            "Using all 32 bytes for MFi challenge (hardware expects 32, got {})",
            challenge.len()
        );
        debug!("MFi challenge hex: {}", hex::encode(challenge));

        if challenge.iter().all(|&b| b == 0x00) {
            return Err(Iap2Error::AuthenticationFailed(
                "Challenge is all zeros - corrupted data".to_string(),
            ));
        }

        if challenge.len() != 32 {
            return Err(Iap2Error::AuthenticationFailed(format!(
                "Challenge length mismatch: expected 32, got {}",
                challenge.len()
            )));
        }

        self.send_response(link, stream, session_id, challenge)
            .await
    }

    async fn send_certificate(
        &self,
        link: &mut Iap2Link,
        stream: &mut Stream,
        session_id: u8,
    ) -> Result<()> {
        info!("Sending authentication certificate");

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let padding = vec![0xa1, 0x00, 0x31, 0x00];
        let certificate = self
            .certificate
            .as_ref()
            .ok_or_else(|| Iap2Error::AuthenticationFailed("Certificate not loaded".to_string()))?;
        let cert_with_padding = [certificate.as_slice(), &padding].concat();

        let data_len = cert_with_padding.len() as u16;

        let mut param = BytesMut::new();
        param.put_u16(data_len);
        param.put_u16(0x0000);
        param.put_slice(&cert_with_padding);

        let mut body = BytesMut::new();
        body.put_u16(IAP2_MSG_ID_AUTH_CERT);
        body.put_slice(&param);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        debug!(
            "Certificate lengths: cert={}, padding=4, data_len={}, msg_len={}",
            certificate.len(),
            data_len,
            msg_len
        );

        link.send_data(stream, session_id, ctrl.freeze()).await?;

        info!(
            "Sent AuthenticationCertificate message ({} bytes certificate including trailer)",
            certificate.len()
        );
        Ok(())
    }

    async fn send_response(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut Stream,
        session_id: u8,
        challenge: &[u8],
    ) -> Result<()> {
        let response = self
            .mfi_provider
            .challenge_response(challenge)
            .await
            .map_err(|e| {
                error!("Failed to perform MFi challenge-response: {}", e);
                Iap2Error::AuthenticationFailed(format!("MFi challenge-response failed: {}", e))
            })?;

        info!(
            "Sending ECDSA signature as authentication response: {} bytes",
            response.len()
        );

        self.send_response_with_payload(link, stream, session_id, &response)
            .await
    }

    async fn send_response_with_payload(
        &self,
        link: &mut Iap2Link,
        stream: &mut Stream,
        session_id: u8,
        response: &[u8],
    ) -> Result<()> {
        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let signature_len = response.len() as u16;

        let mut param = BytesMut::new();
        param.put_u16(2 + 2 + signature_len);
        param.put_u16(0x0000);
        param.put_slice(response);

        let mut body = BytesMut::new();
        body.put_u16(IAP2_MSG_ID_AUTH_RESPONSE);
        body.put_slice(&param);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        debug!(
            "Auth response lengths: resp={} (sig), param_len={}, msg_len={}",
            response.len(),
            2 + 2 + signature_len,
            msg_len
        );
        debug!("Auth response control payload: {}", hex::encode(&body));

        link.send_data(stream, session_id, ctrl.freeze()).await?;

        info!(
            "Sent AuthenticationResponse message ({} bytes signature, total message {} bytes)",
            response.len(),
            msg_len
        );
        Ok(())
    }
}
