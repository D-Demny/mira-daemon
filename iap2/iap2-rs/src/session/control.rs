use bytes::{BufMut, BytesMut};
use tracing::{debug, info};

use crate::error::Result;
use crate::link::Iap2Link;
use crate::types::{DeviceIdentification, HidFunction, NowPlayingConfig, PowerConfig};

pub struct ControlSession {
    identification: DeviceIdentification,
    session_id: u8,
    now_playing_config: NowPlayingConfig,
    power_config: PowerConfig,
}

impl ControlSession {
    pub fn new(
        identification: DeviceIdentification,
        session_id: u8,
        now_playing_config: NowPlayingConfig,
        power_config: PowerConfig,
    ) -> Self {
        ControlSession {
            identification,
            session_id,
            now_playing_config,
            power_config,
        }
    }

    #[allow(unused)]
    pub fn session_id(&self) -> u8 {
        self.session_id
    }

    pub async fn send_identification(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        info!("Sending device identification");

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x1D01); // IdentificationInformation

        // Param 0x00: Name
        let name = format!("{}\0", self.identification.name);
        body.put_u16((name.len() + 4) as u16);
        body.put_u16(0x0000);
        body.put_slice(name.as_bytes());

        // Param 0x01: ModelIdentifier
        let model = format!("{}\0", self.identification.model_identifier);
        body.put_u16((model.len() + 4) as u16);
        body.put_u16(0x0001);
        body.put_slice(model.as_bytes());

        // Param 0x02: Manufacturer
        let manufacturer = format!("{}\0", self.identification.manufacturer);
        body.put_u16((manufacturer.len() + 4) as u16);
        body.put_u16(0x0002);
        body.put_slice(manufacturer.as_bytes());

        // Param 0x03: SerialNumber
        let serial = format!("{}\0", self.identification.serial_number);
        body.put_u16((serial.len() + 4) as u16);
        body.put_u16(0x0003);
        body.put_slice(serial.as_bytes());

        // Param 0x04: FirmwareVersion
        let firmware = format!("{}\0", self.identification.firmware_version);
        body.put_u16((firmware.len() + 4) as u16);
        body.put_u16(0x0004);
        body.put_slice(firmware.as_bytes());

        // Param 0x05: HardwareVersion
        let hardware = format!("{}\0", self.identification.hardware_version);
        body.put_u16((hardware.len() + 4) as u16);
        body.put_u16(0x0005);
        body.put_slice(hardware.as_bytes());

        // Param 0x06: MessagesSentByAccessory
        if !self.identification.messages_sent.is_empty() {
            body.put_u16((self.identification.messages_sent.len() * 2 + 4) as u16);
            body.put_u16(0x0006);
            for msg in &self.identification.messages_sent {
                body.put_u16(*msg);
            }
        }

        // Param 0x07: MessagesReceivedFromDevice
        if !self.identification.messages_received.is_empty() {
            body.put_u16((self.identification.messages_received.len() * 2 + 4) as u16);
            body.put_u16(0x0007);
            for msg in &self.identification.messages_received {
                body.put_u16(*msg);
            }
        }

        // Param 0x08: PowerSourceType
        body.put_u16(4 + 1);
        body.put_u16(0x0008);
        body.put_u8(self.power_config.source_type);

        // Param 0x09: MaximumCurrentDrawnFromDevice
        body.put_u16(4 + 2);
        body.put_u16(0x0009);
        body.put_u16(self.power_config.max_current_ma);

        // Param 0x0A: SupportedExternalAccessoryProtocol
        // (omitted when no EA protocol is declared)
        if !self.identification.ea_protocol_name.is_empty() {
            let protocol_name = format!("{}\0", self.identification.ea_protocol_name);
            let protocol_length = 4 + (4 + 1) + (4 + protocol_name.len()) + (4 + 1);
            body.put_u16(protocol_length as u16);
            body.put_u16(0x000A);
            // Sub-param 0: Protocol identifier
            body.put_u16(4 + 1);
            body.put_u16(0x0000);
            body.put_u8(0x00);
            // Sub-param 1: Protocol name
            body.put_u16((protocol_name.len() + 4) as u16);
            body.put_u16(0x0001);
            body.put_slice(protocol_name.as_bytes());
            // Sub-param 2: Match action
            body.put_u16(4 + 1);
            body.put_u16(0x0002);
            body.put_u8(0x01);
        }

        // Param 0x0B: PreferredAppBundleSeedIdentifier
        if let Some(ref bundle_seed) = self.identification.bundle_seed_id {
            let bundle = format!("{}\0", bundle_seed);
            body.put_u16((bundle.len() + 4) as u16);
            body.put_u16(0x000B);
            body.put_slice(bundle.as_bytes());
        }

        // Param 0x0C: CurrentLanguage
        let current_lang = format!("{}\0", self.identification.current_language);
        body.put_u16((current_lang.len() + 4) as u16);
        body.put_u16(0x000C);
        body.put_slice(current_lang.as_bytes());

        // Param 0x0D: SupportedLanguage
        for lang in &self.identification.supported_languages {
            let lang_str = format!("{}\0", lang);
            body.put_u16((lang_str.len() + 4) as u16);
            body.put_u16(0x000D);
            body.put_slice(lang_str.as_bytes());
        }

        // HID Components
        for (idx, hid) in self.identification.hid_components.iter().enumerate() {
            let param_id = if idx == 0 { 0x0011u16 } else { 0x0012u16 };
            let hid_name = format!("{}\0", hid.name);

            let mut hid_len = 4 + (4 + 2) + (4 + hid_name.len()); // base + id + name
            if let Some(ref data) = hid.extra_data {
                hid_len += 4 + data.len();
            }
            match hid.function {
                HidFunction::None => hid_len += 4,
                HidFunction::MediaPlaybackRemote | HidFunction::Custom(_) => hid_len += 4 + 1,
            }

            body.put_u16(hid_len as u16);
            body.put_u16(param_id);

            // HIDComponentIdentifier
            body.put_u16(4 + 2);
            body.put_u16(0x0000);
            body.put_u16(hid.id);

            // HIDComponentName
            body.put_u16((4 + hid_name.len()) as u16);
            body.put_u16(0x0001);
            body.put_slice(hid_name.as_bytes());

            // HIDComponentFunction
            match hid.function {
                HidFunction::None => {
                    body.put_u16(4);
                    body.put_u16(0x0002);
                }
                HidFunction::MediaPlaybackRemote => {
                    body.put_u16(4 + 1);
                    body.put_u16(0x0002);
                    body.put_u8(0x01);
                }
                HidFunction::Custom(val) => {
                    body.put_u16(4 + 1);
                    body.put_u16(0x0002);
                    body.put_u8(val);
                }
            }

            // Extra data if present
            if let Some(ref data) = hid.extra_data {
                body.put_u16((4 + data.len()) as u16);
                body.put_u16(0x0003);
                body.put_slice(data);
            }
        }

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;

        info!("Sent IdentificationInformation message");
        Ok(())
    }

    pub fn has_ea_protocol(&self) -> bool {
        !self.identification.ea_protocol_name.is_empty()
    }

    pub async fn send_ea_session_request(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        protocol_identifier: u8,
    ) -> Result<()> {
        info!(
            "Sending EA session request for {}",
            self.identification.ea_protocol_name
        );

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0xEA02); // StartExternalAccessoryProtocolSession

        let protocol_name = format!("{}\0", self.identification.ea_protocol_name);
        body.put_u16((protocol_name.len() + 4) as u16);
        body.put_u16(0x0000);
        body.put_slice(protocol_name.as_bytes());

        // ExternalAccessoryProtocolSessionIdentifier
        body.put_u16(4 + 1);
        body.put_u16(0x0001);
        body.put_u8(protocol_identifier);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;
        info!("Sent EA session request");
        Ok(())
    }

    pub async fn send_start_now_playing_updates(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        info!("Sending StartNowPlayingUpdates request");

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x5000); // StartNowPlayingUpdates

        // MediaItemAttributes group
        let mut media_group = BytesMut::new();
        for param_id in &self.now_playing_config.media_item_attributes {
            media_group.put_u16(4);
            media_group.put_u16(*param_id);
        }
        if !media_group.is_empty() {
            body.put_u16((media_group.len() + 4) as u16);
            body.put_u16(0x0000);
            body.put_slice(&media_group);
        }

        // PlaybackAttributes group
        let mut playback_group = BytesMut::new();
        for param_id in &self.now_playing_config.playback_attributes {
            playback_group.put_u16(4);
            playback_group.put_u16(*param_id);
        }
        if !playback_group.is_empty() {
            body.put_u16((playback_group.len() + 4) as u16);
            body.put_u16(0x0001);
            body.put_slice(&playback_group);
        }

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;
        info!("StartNowPlayingUpdates sent");
        Ok(())
    }

    pub async fn send_stop_now_playing_updates(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        info!("Sending StopNowPlayingUpdates request");

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x5002); // StopNowPlayingUpdates

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;
        info!("StopNowPlayingUpdates sent");
        Ok(())
    }

    pub async fn send_keepalive(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
    ) -> Result<()> {
        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0x4157); // RequestStatusUpdate

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;
        debug!("Sent keepalive packet");
        Ok(())
    }

    pub async fn send_app_launch_request(
        &self,
        link: &mut Iap2Link,
        stream: &mut bluer::rfcomm::Stream,
        bundle_id: &str,
    ) -> Result<()> {
        info!("Sending RequestAppLaunch for {}", bundle_id);

        let mut ctrl = BytesMut::new();
        ctrl.put_u8(0x40);
        ctrl.put_u8(0x40);

        let mut body = BytesMut::new();
        body.put_u16(0xEA02); // RequestAppLaunch

        let bundle_bytes = format!("{}\0", bundle_id);
        body.put_u16((bundle_bytes.len() + 4) as u16);
        body.put_u16(0x0000); // AppBundleID
        body.put_slice(bundle_bytes.as_bytes());

        body.put_u16(4 + 1);
        body.put_u16(0x0001);
        body.put_u8(0x01);

        let msg_len = 2 + 2 + body.len() as u16;
        ctrl.put_u16(msg_len);
        ctrl.put_slice(&body);

        link.send_data(stream, self.session_id, ctrl.freeze())
            .await?;
        info!("RequestAppLaunch sent");
        Ok(())
    }
}
