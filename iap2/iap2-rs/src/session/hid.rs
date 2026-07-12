use bytes::{BufMut, BytesMut};
use tracing::{debug, info};

use crate::error::Result;
use crate::link::Iap2Link;
use crate::types::{HidCommand, HidConfig};

const HID_BIT_PLAY: u8 = 0;
const HID_BIT_PAUSE: u8 = 1;
const HID_BIT_NEXT: u8 = 2;
const HID_BIT_PREVIOUS: u8 = 3;
const HID_BIT_SHUFFLE: u8 = 4;
const HID_BIT_REPEAT: u8 = 5;
const HID_BIT_VOLUME_UP: u8 = 6;
const HID_BIT_VOLUME_DOWN: u8 = 7;

pub struct HidRemote {
    started: bool,
    control_session_id: u8,
    config: HidConfig,
}

impl HidRemote {
    pub fn new(control_session_id: u8, config: HidConfig) -> Self {
        HidRemote {
            started: false,
            control_session_id,
            config,
        }
    }

    pub fn is_started(&self) -> bool {
        self.started
    }

    pub async fn ensure_started(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<bool> {
        if !self.started {
            self.send_start(link, stream).await?;
            Ok(true)
        } else {
            Ok(false)
        }
    }

    pub async fn send_start(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        info!("Sending StartHID for media playback remote");

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x6800); // StartHID

        body.put_u16(4 + 2);
        body.put_u16(0x0000);
        body.put_u16(self.config.component_id);

        body.put_u16(4 + 2);
        body.put_u16(0x0001);
        body.put_u16(self.config.vendor_id);

        body.put_u16(4 + 2);
        body.put_u16(0x0002);
        body.put_u16(self.config.product_id);

        body.put_u16((self.config.report_descriptor.len() + 4) as u16);
        body.put_u16(0x0004);
        body.put_slice(&self.config.report_descriptor);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.control_session_id, ctrl.freeze())
            .await?;
        self.started = true;
        info!("StartHID sent successfully");
        Ok(())
    }

    pub async fn send_stop(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x6803); // StopHID
        body.put_u16(4 + 2);
        body.put_u16(0x0000);
        body.put_u16(self.config.component_id);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.control_session_id, ctrl.freeze())
            .await?;
        self.started = false;
        info!("StopHID sent successfully");
        Ok(())
    }

    pub async fn send_command(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        command: HidCommand,
    ) -> Result<()> {
        self.ensure_started(link, stream).await?;

        let bit = match command {
            HidCommand::Play => HID_BIT_PLAY,
            HidCommand::Pause => HID_BIT_PAUSE,
            HidCommand::PlayPause => HID_BIT_PLAY,
            HidCommand::Next => HID_BIT_NEXT,
            HidCommand::Previous => HID_BIT_PREVIOUS,
            HidCommand::Shuffle => HID_BIT_SHUFFLE,
            HidCommand::Repeat => HID_BIT_REPEAT,
            HidCommand::VolumeUp => HID_BIT_VOLUME_UP,
            HidCommand::VolumeDown => HID_BIT_VOLUME_DOWN,
        };

        let pressed = 1u8 << bit;
        self.send_report(link, stream, &[pressed]).await?;
        self.send_report(link, stream, &[0x00]).await?;

        debug!("Sent HID command {:?} (bit {})", command, bit);
        Ok(())
    }

    #[allow(unused)]
    pub async fn send_command_with_playback_state(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        command: HidCommand,
        is_playing: bool,
    ) -> Result<()> {
        self.ensure_started(link, stream).await?;

        let bit = match command {
            HidCommand::PlayPause => {
                if is_playing {
                    HID_BIT_PAUSE
                } else {
                    HID_BIT_PLAY
                }
            }
            _ => return self.send_command(link, stream, command).await,
        };

        let pressed = 1u8 << bit;
        self.send_report(link, stream, &[pressed]).await?;
        self.send_report(link, stream, &[0x00]).await?;

        debug!(
            "Sent HID command {:?} (bit {}, is_playing={})",
            command, bit, is_playing
        );
        Ok(())
    }

    async fn send_report(
        &mut self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        report: &[u8],
    ) -> Result<()> {
        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x6802); // HIDReport
        body.put_u16(4 + 2);
        body.put_u16(0x0000);
        body.put_u16(self.config.component_id);

        body.put_u16((report.len() + 4) as u16);
        body.put_u16(0x0001);
        body.put_slice(report);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.control_session_id, ctrl.freeze())
            .await?;
        Ok(())
    }
}
