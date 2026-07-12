use async_trait::async_trait;

use crate::error::Result;

#[async_trait]
pub trait MfiAuthProvider: Send + Sync {
    async fn read_certificate(&self) -> Result<Vec<u8>>;
    async fn challenge_response(&self, challenge: &[u8]) -> Result<Vec<u8>>;
}

pub struct MockMfiProvider {
    certificate: Vec<u8>,
    response: Vec<u8>,
}

impl MockMfiProvider {
    pub fn new(certificate: Vec<u8>, response: Vec<u8>) -> Self {
        Self {
            certificate,
            response,
        }
    }

    pub fn empty() -> Self {
        Self {
            certificate: vec![],
            response: vec![],
        }
    }
}

#[async_trait]
impl MfiAuthProvider for MockMfiProvider {
    async fn read_certificate(&self) -> Result<Vec<u8>> {
        Ok(self.certificate.clone())
    }

    async fn challenge_response(&self, _challenge: &[u8]) -> Result<Vec<u8>> {
        Ok(self.response.clone())
    }
}
